package cli

import (
	"flag"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"

	"github.com/israel-duff/xenibase/internal/api/client"
)

// A dedicated project's read replicas (V4 §7).

func (a *App) replicasList(args []string) error {
	pos, err := parse(flag.NewFlagSet("replicas list", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	if err := need(pos, 1, "replicas list <project>"); err != nil {
		return err
	}
	p, err := a.project(pos[0])
	if err != nil {
		return err
	}
	c, cancel := ctx()
	defer cancel()
	r, err := a.api.ListProjectReplicasWithResponse(c, p.Id)
	if err := check(r, err); err != nil {
		return err
	}
	return a.emit(r.JSON200, func(w io.Writer) {
		o := r.JSON200
		if len(o.Replicas) == 0 {
			fmt.Fprintf(w, "No read replicas (up to %d): xenibase replicas create %s\n", o.MaxReplicas, p.Name)
			return
		}
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tNODE\tREGION\tSTATUS\tIN ROTATION\tLAG\tSTANDBY")
		for _, rp := range o.Replicas {
			lag := "-"
			if rp.LagMs != nil {
				lag = (time.Duration(*rp.LagMs) * time.Millisecond).Round(100 * time.Millisecond).String()
			}
			in := "no"
			if rp.InRotation {
				in = "yes"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", rp.Id, rp.NodeName, rp.Region, rp.Status, in, lag, standbyText(rp))
		}
		_ = tw.Flush()
		if o.ReadUrl != nil {
			fmt.Fprintf(w, "Read-only URL: %s (your project password)\n", *o.ReadUrl)
		}
		fmt.Fprintf(w, "A replica more than %s behind leaves rotation until it catches up.\n", time.Duration(o.MaxLagMs)*time.Millisecond)
	})
}

func (a *App) replicasCreate(args []string) error {
	fs := flag.NewFlagSet("replicas create", flag.ContinueOnError)
	node := fs.String("node", "", "the id of the node for the replica (default: the least loaded)")
	region := fs.String("region", "", "the region it goes in (default: the project's)")
	feedback, delay := standbyFlags(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if err := need(pos, 1, "replicas create <project> [--node <id>] [--region <r>] [--feedback on|off] [--standby-delay <s>]"); err != nil {
		return err
	}
	p, err := a.project(pos[0])
	if err != nil {
		return err
	}
	req := client.ReplicaCreateRequest{}
	if *node != "" {
		id, err := uuid.Parse(*node)
		if err != nil {
			return usageErrorf("--node takes the node's id")
		}
		req.NodeId = &id
	}
	if *region != "" {
		req.Region = region
	}
	fb, d, err := standbyValues(fs, *feedback, *delay)
	if err != nil {
		return err
	}
	req.HotStandbyFeedback, req.MaxStandbyDelaySeconds = fb, d
	c, cancel := ctx()
	defer cancel()
	r, err := a.api.CreateProjectReplicaWithResponse(c, p.Id, req)
	if err := check(r, err); err != nil {
		return err
	}
	return a.followOp(r.JSON202, "the read replica of "+p.Name+" streams; reads through "+p.Name+"'s _ro route use it once it keeps up")
}

// replicaArgs resolves <project> <replica-id>.
func (a *App) replicaArgs(pos []string, usage string) (client.Project, uuid.UUID, error) {
	if err := need(pos, 2, usage); err != nil {
		return client.Project{}, uuid.Nil, err
	}
	p, err := a.project(pos[0])
	if err != nil {
		return p, uuid.Nil, err
	}
	id, err := uuid.Parse(pos[1])
	if err != nil {
		return p, uuid.Nil, usageErrorf("the replica is named by its id (xenibase replicas list %s)", pos[0])
	}
	return p, id, nil
}

func (a *App) replicasDelete(args []string) error {
	pos, err := parse(flag.NewFlagSet("replicas delete", flag.ContinueOnError), args)
	if err != nil {
		return err
	}
	p, id, err := a.replicaArgs(pos, "replicas delete <project> <replica-id>")
	if err != nil {
		return err
	}
	c, cancel := ctx()
	defer cancel()
	r, err := a.api.DeleteProjectReplicaWithResponse(c, p.Id, id)
	if err := check(r, err); err != nil {
		return err
	}
	return a.followOp(r.JSON202, "the read replica is removed")
}

func (a *App) replicasDetach(args []string) error {
	fs := flag.NewFlagSet("replicas detach", flag.ContinueOnError)
	name := fs.String("name", "", "the new project's name")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	p, id, err := a.replicaArgs(pos, "replicas detach <project> <replica-id> --name <new project>")
	if err != nil {
		return err
	}
	if *name == "" {
		return usageErrorf("name the new project with --name")
	}
	c, cancel := ctx()
	defer cancel()
	r, err := a.api.DetachProjectReplicaWithResponse(c, p.Id, id, client.ReplicaDetachRequest{Name: *name})
	if err := check(r, err); err != nil {
		return err
	}
	cr := r.JSON202
	if !a.json {
		fmt.Fprintf(a.Stderr, "Detaching into %s (operation %s)…\n", cr.Project.Name, cr.Operation.Id)
	}
	if !a.noWait {
		if err := a.follow(cr.Operation.Id, !a.json); err != nil {
			return err
		}
	}
	return a.emit(cr, func(w io.Writer) {
		fmt.Fprintf(w, "Project:\t%s (%s)\n", cr.Project.Name, cr.Project.Id)
		fmt.Fprintf(w, "Pooled URL:\t%s\n", cr.Connection.PooledUrl)
		fmt.Fprintf(w, "Session URL:\t%s\n", cr.Connection.SessionUrl)
		fmt.Fprintf(w, "Password:\t%s\n", cr.Password)
		fmt.Fprintln(w, "\nThe password is shown once: store it now.")
	})
}

// standbyFlags are a replica's standby settings on the command line.
func standbyFlags(fs *flag.FlagSet) (*string, *int) {
	return fs.String("feedback", "", "hot_standby_feedback: on or off (default off)"),
		fs.Int("standby-delay", 30, "seconds replay waits for a conflicting query before cancelling it, 0 to 900")
}

// standbyValues returns the flags that were given (nil for the others).
func standbyValues(fs *flag.FlagSet, feedback string, delay int) (*bool, *int, error) {
	var fb *bool
	switch feedback {
	case "":
	case "on", "off":
		v := feedback == "on"
		fb = &v
	default:
		return nil, nil, usageErrorf("--feedback takes on or off")
	}
	var d *int
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "standby-delay" {
			d = &delay
		}
	})
	return fb, d, nil
}

func standbyText(r client.ReadReplica) string {
	fb := "feedback off"
	if r.HotStandbyFeedback {
		fb = "feedback on"
	}
	t := fmt.Sprintf("%s, waits %ds", fb, r.MaxStandbyDelaySeconds)
	if r.SettingsError != nil {
		t += " (not applied yet)"
	}
	return t
}

func (a *App) replicasSet(args []string) error {
	fs := flag.NewFlagSet("replicas set", flag.ContinueOnError)
	feedback, delay := standbyFlags(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	p, id, err := a.replicaArgs(pos, "replicas set <project> <replica-id> [--feedback on|off] [--standby-delay <s>]")
	if err != nil {
		return err
	}
	fb, d, err := standbyValues(fs, *feedback, *delay)
	if err != nil {
		return err
	}
	if fb == nil && d == nil {
		return usageErrorf("give --feedback, --standby-delay or both")
	}
	c, cancel := ctx()
	defer cancel()
	r, err := a.api.UpdateProjectReplicaWithResponse(c, p.Id, id, client.ReplicaSettings{HotStandbyFeedback: fb, MaxStandbyDelaySeconds: d})
	if err := check(r, err); err != nil {
		return err
	}
	return a.emit(r.JSON200, func(w io.Writer) {
		rp := r.JSON200
		fmt.Fprintf(w, "Replica %s on %s: %s\n", rp.Id, rp.NodeName, standbyText(*rp))
		if rp.SettingsError != nil {
			fmt.Fprintf(w, "Not applied yet (%s); Xenibase retries while it streams.\n", *rp.SettingsError)
		}
	})
}
