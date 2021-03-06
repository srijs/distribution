package handlers

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"

	"github.com/docker/distribution"
	ctxu "github.com/docker/distribution/context"
	"github.com/docker/distribution/digest"
	"github.com/docker/distribution/registry/api/v2"
	"github.com/gorilla/handlers"
)

// layerUploadDispatcher constructs and returns the layer upload handler for
// the given request context.
func layerUploadDispatcher(ctx *Context, r *http.Request) http.Handler {
	luh := &layerUploadHandler{
		Context: ctx,
		UUID:    getUploadUUID(ctx),
	}

	handler := http.Handler(handlers.MethodHandler{
		"POST": http.HandlerFunc(luh.StartLayerUpload),
		"GET":  http.HandlerFunc(luh.GetUploadStatus),
		"HEAD": http.HandlerFunc(luh.GetUploadStatus),
		// TODO(stevvooe): Must implement patch support.
		// "PATCH":    http.HandlerFunc(luh.PutLayerChunk),
		"PUT":    http.HandlerFunc(luh.PutLayerUploadComplete),
		"DELETE": http.HandlerFunc(luh.CancelLayerUpload),
	})

	if luh.UUID != "" {
		state, err := hmacKey(ctx.Config.HTTP.Secret).unpackUploadState(r.FormValue("_state"))
		if err != nil {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctxu.GetLogger(ctx).Infof("error resolving upload: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				luh.Errors.Push(v2.ErrorCodeBlobUploadInvalid, err)
			})
		}
		luh.State = state

		if state.Name != ctx.Repository.Name() {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctxu.GetLogger(ctx).Infof("mismatched repository name in upload state: %q != %q", state.Name, luh.Repository.Name())
				w.WriteHeader(http.StatusBadRequest)
				luh.Errors.Push(v2.ErrorCodeBlobUploadInvalid, err)
			})
		}

		if state.UUID != luh.UUID {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ctxu.GetLogger(ctx).Infof("mismatched uuid in upload state: %q != %q", state.UUID, luh.UUID)
				w.WriteHeader(http.StatusBadRequest)
				luh.Errors.Push(v2.ErrorCodeBlobUploadInvalid, err)
			})
		}

		layers := ctx.Repository.Layers()
		upload, err := layers.Resume(luh.UUID)
		if err != nil {
			ctxu.GetLogger(ctx).Errorf("error resolving upload: %v", err)
			if err == distribution.ErrLayerUploadUnknown {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusNotFound)
					luh.Errors.Push(v2.ErrorCodeBlobUploadUnknown, err)
				})
			}

			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				luh.Errors.Push(v2.ErrorCodeUnknown, err)
			})
		}
		luh.Upload = upload

		if state.Offset > 0 {
			// Seek the layer upload to the correct spot if it's non-zero.
			// These error conditions should be rare and demonstrate really
			// problems. We basically cancel the upload and tell the client to
			// start over.
			if nn, err := upload.Seek(luh.State.Offset, os.SEEK_SET); err != nil {
				defer upload.Close()
				ctxu.GetLogger(ctx).Infof("error seeking layer upload: %v", err)
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusBadRequest)
					luh.Errors.Push(v2.ErrorCodeBlobUploadInvalid, err)
					upload.Cancel()
				})
			} else if nn != luh.State.Offset {
				defer upload.Close()
				ctxu.GetLogger(ctx).Infof("seek to wrong offest: %d != %d", nn, luh.State.Offset)
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusBadRequest)
					luh.Errors.Push(v2.ErrorCodeBlobUploadInvalid, err)
					upload.Cancel()
				})
			}
		}

		handler = closeResources(handler, luh.Upload)
	}

	return handler
}

// layerUploadHandler handles the http layer upload process.
type layerUploadHandler struct {
	*Context

	// UUID identifies the upload instance for the current request.
	UUID string

	Upload distribution.LayerUpload

	State layerUploadState
}

// StartLayerUpload begins the layer upload process and allocates a server-
// side upload session.
func (luh *layerUploadHandler) StartLayerUpload(w http.ResponseWriter, r *http.Request) {
	layers := luh.Repository.Layers()
	upload, err := layers.Upload()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError) // Error conditions here?
		luh.Errors.Push(v2.ErrorCodeUnknown, err)
		return
	}

	luh.Upload = upload
	defer luh.Upload.Close()

	if err := luh.layerUploadResponse(w, r); err != nil {
		w.WriteHeader(http.StatusInternalServerError) // Error conditions here?
		luh.Errors.Push(v2.ErrorCodeUnknown, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// GetUploadStatus returns the status of a given upload, identified by uuid.
func (luh *layerUploadHandler) GetUploadStatus(w http.ResponseWriter, r *http.Request) {
	if luh.Upload == nil {
		w.WriteHeader(http.StatusNotFound)
		luh.Errors.Push(v2.ErrorCodeBlobUploadUnknown)
		return
	}

	if err := luh.layerUploadResponse(w, r); err != nil {
		w.WriteHeader(http.StatusInternalServerError) // Error conditions here?
		luh.Errors.Push(v2.ErrorCodeUnknown, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// PutLayerUploadComplete takes the final request of a layer upload. The final
// chunk may include all the layer data, the final chunk of layer data or no
// layer data. Any data provided is received and verified. If successful, the
// layer is linked into the blob store and 201 Created is returned with the
// canonical url of the layer.
func (luh *layerUploadHandler) PutLayerUploadComplete(w http.ResponseWriter, r *http.Request) {
	if luh.Upload == nil {
		w.WriteHeader(http.StatusNotFound)
		luh.Errors.Push(v2.ErrorCodeBlobUploadUnknown)
		return
	}

	dgstStr := r.FormValue("digest") // TODO(stevvooe): Support multiple digest parameters!

	if dgstStr == "" {
		// no digest? return error, but allow retry.
		w.WriteHeader(http.StatusNotFound)
		luh.Errors.Push(v2.ErrorCodeDigestInvalid, "digest missing")
		return
	}

	dgst, err := digest.ParseDigest(dgstStr)
	if err != nil {
		// no digest? return error, but allow retry.
		w.WriteHeader(http.StatusNotFound)
		luh.Errors.Push(v2.ErrorCodeDigestInvalid, "digest parsing failed")
		return
	}

	// TODO(stevvooe): Check the incoming range header here, per the
	// specification. LayerUpload should be seeked (sought?) to that position.

	// Read in the final chunk, if any.
	io.Copy(luh.Upload, r.Body)

	layer, err := luh.Upload.Finish(dgst)
	if err != nil {
		switch err := err.(type) {
		case distribution.ErrLayerInvalidDigest:
			w.WriteHeader(http.StatusBadRequest)
			luh.Errors.Push(v2.ErrorCodeDigestInvalid, err)
		default:
			ctxu.GetLogger(luh).Errorf("unknown error completing upload: %#v", err)
			w.WriteHeader(http.StatusInternalServerError)
			luh.Errors.Push(v2.ErrorCodeUnknown, err)
		}

		// Clean up the backend layer data if there was an error.
		if err := luh.Upload.Cancel(); err != nil {
			// If the cleanup fails, all we can do is observe and report.
			ctxu.GetLogger(luh).Errorf("error canceling upload after error: %v", err)
		}

		return
	}

	// Build our canonical layer url
	layerURL, err := luh.urlBuilder.BuildBlobURL(luh.Repository.Name(), layer.Digest())
	if err != nil {
		luh.Errors.Push(v2.ErrorCodeUnknown, err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Header().Set("Location", layerURL)
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusCreated)
}

// CancelLayerUpload cancels an in-progress upload of a layer.
func (luh *layerUploadHandler) CancelLayerUpload(w http.ResponseWriter, r *http.Request) {
	if luh.Upload == nil {
		w.WriteHeader(http.StatusNotFound)
		luh.Errors.Push(v2.ErrorCodeBlobUploadUnknown)
		return
	}

	if err := luh.Upload.Cancel(); err != nil {
		ctxu.GetLogger(luh).Errorf("error encountered canceling upload: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		luh.Errors.PushErr(err)
	}

	w.WriteHeader(http.StatusNoContent)
}

// layerUploadResponse provides a standard request for uploading layers and
// chunk responses. This sets the correct headers but the response status is
// left to the caller.
func (luh *layerUploadHandler) layerUploadResponse(w http.ResponseWriter, r *http.Request) error {

	offset, err := luh.Upload.Seek(0, os.SEEK_CUR)
	if err != nil {
		ctxu.GetLogger(luh).Errorf("unable get current offset of layer upload: %v", err)
		return err
	}

	// TODO(stevvooe): Need a better way to manage the upload state automatically.
	luh.State.Name = luh.Repository.Name()
	luh.State.UUID = luh.Upload.UUID()
	luh.State.Offset = offset
	luh.State.StartedAt = luh.Upload.StartedAt()

	token, err := hmacKey(luh.Config.HTTP.Secret).packUploadState(luh.State)
	if err != nil {
		ctxu.GetLogger(luh).Infof("error building upload state token: %s", err)
		return err
	}

	uploadURL, err := luh.urlBuilder.BuildBlobUploadChunkURL(
		luh.Repository.Name(), luh.Upload.UUID(),
		url.Values{
			"_state": []string{token},
		})
	if err != nil {
		ctxu.GetLogger(luh).Infof("error building upload url: %s", err)
		return err
	}

	w.Header().Set("Location", uploadURL)
	w.Header().Set("Content-Length", "0")
	w.Header().Set("Range", fmt.Sprintf("0-%d", luh.State.Offset))

	return nil
}
