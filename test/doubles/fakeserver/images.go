package fakeserver

import (
	"net/http"
	"sort"

	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
)

// The image cache, as much of it as a program reading and
// clearing it needs: what PutImage left there, each ready, read and deleted
// as the server does. The double pulls nothing.

// PutImage keeps an image in the cache, as a pull would
// have left it.
func (d *Server) PutImage(id, ref string, size int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.images[id] = &genv1.Image{Id: id, Ref: ref, Digest: "sha256:" + id, State: genv1.ImageStateReady,
		Size: &size, CreatedAt: d.now().UTC()}
}

// imageIn is an image with what boots from it now, or nil.
// Called with mu held.
func (d *Server) imageIn(id string) *genv1.Image {
	image, ok := d.images[id]
	if !ok {
		return nil
	}
	out := *image
	booted := 0
	for _, s := range d.sandboxes {
		if s.record.Spec.Image == image.Ref && isRunning(s.record.State) {
			booted++
		}
	}
	out.Sandboxes = &booted
	return &out
}

func (d *Server) listImages(w http.ResponseWriter, _ *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	page := genv1.ImagePage{Items: []genv1.Image{}}
	for id := range d.images {
		page.Items = append(page.Items, *d.imageIn(id))
	}
	sort.Slice(page.Items, func(i, j int) bool { return page.Items[i].Id < page.Items[j].Id })
	reply(w, http.StatusOK, page)
}

func (d *Server) getImage(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	image := d.imageIn(r.PathValue("imageId"))
	if image == nil {
		refuse(w, http.StatusNotFound, genv1.ErrorCodeNotFound, "no image "+r.PathValue("imageId"))
		return
	}
	reply(w, http.StatusOK, image)
}

// deleteImage drops it, unless a sandbox runs from it; one already gone is
// gone, as the server says.
func (d *Server) deleteImage(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("imageId")
	d.mu.Lock()
	defer d.mu.Unlock()
	if image := d.imageIn(id); image != nil && *image.Sandboxes > 0 {
		refuse(w, http.StatusConflict, genv1.ErrorCodeConflict, "a sandbox is booted from "+image.Ref)
		return
	}
	delete(d.images, id)
	w.WriteHeader(http.StatusNoContent)
}
