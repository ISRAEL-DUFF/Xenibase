package edge

import "github.com/israel-duff/xenibase/internal/edgeapi"

// LoadForStorageTest installs project configurations as if from the feed
// and marks the edge ready, for the storage tests in package edge_test.
func (e *Edge) LoadForStorageTest(ps ...edgeapi.Project) {
	e.apply(ps)
	e.mu.Lock()
	e.ready = true
	e.mu.Unlock()
}

// SetMaxOpenUploads changes the cap on a project's open multipart uploads;
// restore puts it back.
func SetMaxOpenUploads(n int) (restore func()) {
	old := maxOpenUploads
	maxOpenUploads = n
	return func() { maxOpenUploads = old }
}
