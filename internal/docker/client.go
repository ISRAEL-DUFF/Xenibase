// Package docker is a small client for the Docker Engine API, covering what
// xenibase-agent needs for instances: images, volumes, containers, and exec.
// It talks HTTP over the daemon's Unix socket (or TCP) at a pinned API
// version, so the agent carries no SDK dependency tree.
package docker

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// APIVersion is the Engine API version the agent is written against
// (Docker 24+). A daemon that no longer accepts it (Docker 29 raised its
// minimum to 1.44) is spoken to at the oldest version it does accept.
const APIVersion = "v1.43"

// ErrNotFound is returned for a missing container, volume, or image.
var ErrNotFound = errors.New("not found")

// Client talks to one Docker daemon.
type Client struct {
	http *http.Client
	host string // scheme and authority, without an API version

	mu      sync.Mutex
	version string // negotiated on first use
}

// New returns a client for host: "unix:///var/run/docker.sock" (the
// default when empty) or "tcp://host:port".
func New(host string) (*Client, error) {
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}
	u, err := url.Parse(host)
	if err != nil {
		return nil, fmt.Errorf("docker host %q: %w", host, err)
	}
	tr := &http.Transport{}
	host = "http://docker"
	switch u.Scheme {
	case "unix":
		path := u.Path
		tr.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		}
	case "tcp":
		host = "http://" + u.Host
	default:
		return nil, fmt.Errorf("docker host %q: want unix:// or tcp://", host)
	}
	return &Client{http: &http.Client{Transport: tr}, host: host}, nil
}

// pickAPIVersion returns the version to request from a daemon that accepts
// lo through hi: APIVersion when it is in range, else the daemon's minimum.
func pickAPIVersion(lo, hi string) (string, error) {
	want := strings.TrimPrefix(APIVersion, "v")
	if lo == "" || compareVersions(want, lo) >= 0 {
		return APIVersion, nil
	}
	if hi != "" && compareVersions(lo, hi) > 0 {
		return "", fmt.Errorf("docker daemon reports API versions %s to %s", lo, hi)
	}
	return "v" + lo, nil
}

// compareVersions compares dotted numeric versions such as "1.43" and "1.9".
func compareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// apiBase returns the versioned base URL, asking the daemon which versions
// it accepts the first time. If it can't be asked, APIVersion is used and the
// next call tries again.
func (c *Client) apiBase(ctx context.Context) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.version != "" {
		return c.host + "/" + c.version
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.host+"/version", nil)
	if err != nil {
		return c.host + "/" + APIVersion
	}
	res, err := c.http.Do(req)
	if err != nil {
		return c.host + "/" + APIVersion
	}
	defer res.Body.Close()
	var v struct {
		APIVersion    string `json:"ApiVersion"`
		MinAPIVersion string `json:"MinAPIVersion"`
	}
	if res.StatusCode != http.StatusOK || json.NewDecoder(res.Body).Decode(&v) != nil {
		return c.host + "/" + APIVersion
	}
	picked, err := pickAPIVersion(v.MinAPIVersion, v.APIVersion)
	if err != nil {
		picked = APIVersion
	}
	c.version = picked
	return c.host + "/" + c.version
}

// APIError is an error response from the daemon.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("docker: %s (HTTP %d)", e.Message, e.Status) }

func (c *Client) do(ctx context.Context, method, path string, query url.Values, in, out any) error {
	res, err := c.raw(ctx, method, path, query, in)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, res.Body)
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}

// raw sends a request and returns the response if it succeeded (2xx or
// 304); the caller closes the body.
func (c *Client) raw(ctx context.Context, method, path string, query url.Values, in any) (*http.Response, error) {
	var body io.Reader
	ctype := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body, ctype = bytes.NewReader(b), "application/json"
	}
	return c.send(ctx, method, path, query, body, ctype)
}

func (c *Client) send(ctx context.Context, method, path string, query url.Values, body io.Reader, ctype string) (*http.Response, error) {
	u := c.apiBase(ctx) + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("docker %s %s: %w", method, path, err)
	}
	if res.StatusCode >= 300 && res.StatusCode != http.StatusNotModified {
		defer res.Body.Close()
		var e struct {
			Message string `json:"message"`
		}
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
		_ = json.Unmarshal(raw, &e)
		if e.Message == "" {
			e.Message = strings.TrimSpace(string(raw))
		}
		if res.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, e.Message)
		}
		return nil, &APIError{Status: res.StatusCode, Message: e.Message}
	}
	return res, nil
}

// Ping checks the daemon answers.
func (c *Client) Ping(ctx context.Context) error {
	res, err := c.raw(ctx, http.MethodGet, "/_ping", nil, nil)
	if err != nil {
		return err
	}
	return res.Body.Close()
}

// ---- Images --------------------------------------------------------------

// ImageExists reports whether ref is present locally.
func (c *Client) ImageExists(ctx context.Context, ref string) (bool, error) {
	err := c.do(ctx, http.MethodGet, "/images/"+url.PathEscape(ref)+"/json", nil, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// Image is what InspectImage returns.
type Image struct {
	ID     string `json:"Id"`
	Config struct {
		Env    []string          `json:"Env"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

// Env returns the value of an environment variable the image sets.
func (i Image) Env(name string) string {
	for _, e := range i.Config.Env {
		if k, v, ok := strings.Cut(e, "="); ok && k == name {
			return v
		}
	}
	return ""
}

// InspectImage describes a local image by tag or ID.
func (c *Client) InspectImage(ctx context.Context, ref string) (Image, error) {
	var out Image
	err := c.do(ctx, http.MethodGet, "/images/"+url.PathEscape(ref)+"/json", nil, nil, &out)
	return out, err
}

// Pull fetches ref from its registry.
func (c *Client) Pull(ctx context.Context, ref string) error {
	name, tag := ref, "latest"
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		name, tag = ref[:i], ref[i+1:]
	}
	res, err := c.raw(ctx, http.MethodPost, "/images/create", url.Values{"fromImage": {name}, "tag": {tag}}, nil)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	// The progress stream reports failures in-band.
	dec := json.NewDecoder(res.Body)
	for {
		var m struct {
			Error string `json:"error"`
		}
		if err := dec.Decode(&m); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}
		if m.Error != "" {
			return fmt.Errorf("pull %s: %s", ref, m.Error)
		}
	}
}

// ---- Volumes ---------------------------------------------------------------

// CreateVolume creates a named local volume (idempotent).
func (c *Client) CreateVolume(ctx context.Context, name string, labels map[string]string) error {
	return c.do(ctx, http.MethodPost, "/volumes/create", nil, map[string]any{"Name": name, "Labels": labels}, nil)
}

// RemoveVolume deletes a volume; a missing one is not an error.
func (c *Client) RemoveVolume(ctx context.Context, name string) error {
	err := c.do(ctx, http.MethodDelete, "/volumes/"+url.PathEscape(name), url.Values{"force": {"true"}}, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// ---- Containers ------------------------------------------------------------

// ContainerConfig is the subset of the create body Xenibase uses.
type ContainerConfig struct {
	Image        string              `json:"Image"`
	Cmd          []string            `json:"Cmd,omitempty"`
	Entrypoint   []string            `json:"Entrypoint,omitempty"`
	Env          []string            `json:"Env,omitempty"`
	User         string              `json:"User,omitempty"`
	Labels       map[string]string   `json:"Labels,omitempty"`
	ExposedPorts map[string]struct{} `json:"ExposedPorts,omitempty"`
	StopSignal   string              `json:"StopSignal,omitempty"`
	StopTimeout  *int                `json:"StopTimeout,omitempty"`
	HostConfig   HostConfig          `json:"HostConfig"`
	Networking   *NetworkingConfig   `json:"NetworkingConfig,omitempty"`
}

// HostConfig holds limits, mounts, ports, and the restart policy.
type HostConfig struct {
	Mounts        []Mount                  `json:"Mounts,omitempty"`
	NanoCPUs      int64                    `json:"NanoCpus,omitempty"`
	Memory        int64                    `json:"Memory,omitempty"`
	PortBindings  map[string][]PortBinding `json:"PortBindings,omitempty"`
	RestartPolicy *RestartPolicy           `json:"RestartPolicy,omitempty"`
	NetworkMode   string                   `json:"NetworkMode,omitempty"`
	ShmSize       int64                    `json:"ShmSize,omitempty"`
	AutoRemove    bool                     `json:"AutoRemove,omitempty"`
}

// Mount is a volume mount.
type Mount struct {
	Type   string `json:"Type"` // "volume"
	Source string `json:"Source"`
	Target string `json:"Target"`
}

// PortBinding publishes a container port on the host.
type PortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

// RestartPolicy says when the daemon restarts the container.
type RestartPolicy struct {
	Name string `json:"Name"`
}

// NetworkingConfig attaches the container to networks at creation.
type NetworkingConfig struct {
	EndpointsConfig map[string]EndpointSettings `json:"EndpointsConfig"`
}

// EndpointSettings for one network.
type EndpointSettings struct {
	Aliases []string `json:"Aliases,omitempty"`
}

// CreateContainer creates a container named name and returns its ID.
func (c *Client) CreateContainer(ctx context.Context, name string, cfg ContainerConfig) (string, error) {
	var out struct {
		ID string `json:"Id"`
	}
	err := c.do(ctx, http.MethodPost, "/containers/create", url.Values{"name": {name}}, cfg, &out)
	return out.ID, err
}

// Container is what InspectContainer returns.
type Container struct {
	ID string `json:"Id"`
	// ImageID is the image the container was created from, which a tag
	// may no longer point to.
	ImageID      string `json:"Image"`
	Name         string `json:"Name"`
	RestartCount int    `json:"RestartCount"`
	State        struct {
		Status   string `json:"Status"`
		Running  bool   `json:"Running"`
		ExitCode int    `json:"ExitCode"`
		Error    string `json:"Error"`
		Health   *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
	Config struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	NetworkSettings struct {
		Ports    map[string][]PortBinding `json:"Ports"`
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

// ContainerSummary is one entry of ListContainers.
type ContainerSummary struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	State  string            `json:"State"`
	Labels map[string]string `json:"Labels"`
}

// ListContainers lists the containers, running or not, that carry label.
func (c *Client) ListContainers(ctx context.Context, label string) ([]ContainerSummary, error) {
	filters, err := json.Marshal(map[string][]string{"label": {label}})
	if err != nil {
		return nil, err
	}
	var out []ContainerSummary
	err = c.do(ctx, http.MethodGet, "/containers/json", url.Values{"all": {"true"}, "filters": {string(filters)}}, nil, &out)
	return out, err
}

// File is one file CopyTo writes.
type File struct {
	Name string // relative to the directory; a trailing "/" makes a directory
	Mode int64
	Data []byte
}

// CopyTo writes files into dir of a container, created or running (the
// archive endpoint); dir must exist in the image.
func (c *Client) CopyTo(ctx context.Context, id, dir string, files []File) error {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, f := range files {
		h := &tar.Header{Name: f.Name, Mode: f.Mode, Size: int64(len(f.Data)), ModTime: time.Now(), Typeflag: tar.TypeReg}
		if dir, ok := strings.CutSuffix(f.Name, "/"); ok { // a directory
			h.Name, h.Size, h.Typeflag = dir, 0, tar.TypeDir
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if _, err := tw.Write(f.Data); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	res, err := c.send(ctx, http.MethodPut, "/containers/"+url.PathEscape(id)+"/archive", url.Values{"path": {dir}}, &buf, "application/x-tar")
	if err != nil {
		return err
	}
	return res.Body.Close()
}

// InspectContainer returns a container by name or ID (ErrNotFound if absent).
func (c *Client) InspectContainer(ctx context.Context, id string) (Container, error) {
	var out Container
	err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/json", nil, nil, &out)
	return out, err
}

// StartContainer starts a container; an already running one is fine.
func (c *Client) StartContainer(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/start", nil, nil, nil)
}

// StopContainer stops a container, giving it timeout to shut down cleanly.
func (c *Client) StopContainer(ctx context.Context, id string, timeout time.Duration) error {
	return c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/stop",
		url.Values{"t": {fmt.Sprint(int(timeout.Seconds()))}}, nil, nil)
}

// RenameContainer gives a container another name.
func (c *Client) RenameContainer(ctx context.Context, id, name string) error {
	return c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/rename", url.Values{"name": {name}}, nil, nil)
}

// RemoveContainer force-removes a container (not its named volumes); a
// missing one is not an error.
func (c *Client) RemoveContainer(ctx context.Context, id string) error {
	// v: also the anonymous volumes an image declares (named ones stay).
	err := c.do(ctx, http.MethodDelete, "/containers/"+url.PathEscape(id), url.Values{"force": {"true"}, "v": {"true"}}, nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// WaitContainer blocks until the container exits and returns its exit code.
func (c *Client) WaitContainer(ctx context.Context, id string) (int, error) {
	var out struct {
		StatusCode int `json:"StatusCode"`
		Error      *struct {
			Message string `json:"Message"`
		} `json:"Error"`
	}
	if err := c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/wait", nil, nil, &out); err != nil {
		return -1, err
	}
	if out.Error != nil && out.Error.Message != "" {
		return out.StatusCode, errors.New(out.Error.Message)
	}
	return out.StatusCode, nil
}

// Logs returns the last lines of a container's stdout and stderr.
func (c *Client) Logs(ctx context.Context, id string, tail int) (string, error) {
	res, err := c.raw(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/logs",
		url.Values{"stdout": {"1"}, "stderr": {"1"}, "tail": {fmt.Sprint(tail)}}, nil)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var out bytes.Buffer
	if err := demux(res.Body, &out, &out); err != nil {
		return out.String(), err
	}
	return out.String(), nil
}

// ---- Exec ------------------------------------------------------------------

// ExecResult is a finished command.
type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// Exec runs cmd in a running container as user and waits for it.
func (c *Client) Exec(ctx context.Context, id, user string, env, cmd []string) (ExecResult, error) {
	var created struct {
		ID string `json:"Id"`
	}
	if err := c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/exec", nil, map[string]any{
		"Cmd": cmd, "User": user, "Env": env, "AttachStdout": true, "AttachStderr": true,
	}, &created); err != nil {
		return ExecResult{}, err
	}
	res, err := c.raw(ctx, http.MethodPost, "/exec/"+created.ID+"/start", nil, map[string]any{"Detach": false, "Tty": false})
	if err != nil {
		return ExecResult{}, err
	}
	var stdout, stderr bytes.Buffer
	err = demux(res.Body, &capped{w: &stdout}, &capped{w: &stderr})
	_ = res.Body.Close()
	if err != nil {
		return ExecResult{}, err
	}
	var info struct {
		ExitCode int  `json:"ExitCode"`
		Running  bool `json:"Running"`
	}
	if err := c.do(ctx, http.MethodGet, "/exec/"+created.ID+"/json", nil, nil, &info); err != nil {
		return ExecResult{}, err
	}
	return ExecResult{ExitCode: info.ExitCode, Stdout: stdout.String(), Stderr: stderr.String()}, nil
}

// capped keeps the first 1 MiB written to it.
type capped struct {
	w *bytes.Buffer
}

func (c *capped) Write(p []byte) (int, error) {
	if room := 1<<20 - c.w.Len(); room > 0 {
		if len(p) > room {
			c.w.Write(p[:room])
		} else {
			c.w.Write(p)
		}
	}
	return len(p), nil
}

// demux splits Docker's multiplexed stdout/stderr stream (8-byte frame
// headers: stream, 0, 0, 0, uint32 length).
func demux(r io.Reader, stdout, stderr io.Writer) error {
	br := bufio.NewReader(r)
	var hdr [8]byte
	for {
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}
		n := int64(binary.BigEndian.Uint32(hdr[4:]))
		w := stdout
		if hdr[0] == 2 {
			w = stderr
		}
		if _, err := io.CopyN(w, br, n); err != nil {
			return err
		}
	}
}
