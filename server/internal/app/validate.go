package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	nethttpmiddleware "github.com/oapi-codegen/nethttp-middleware"

	"github.com/oldsj/mainloop/server/internal/httpx"
)

const maxMessage = 300

// validationDetail is one entry of the contract's HTTPValidationError.
type validationDetail struct {
	Loc  []string `json:"loc"`
	Msg  string   `json:"msg"`
	Type string   `json:"type"`
}

// requestValidator checks every request against the contract before routing to a handler:
// unknown paths 404, wrong methods 405, and bad parameters or bodies 422 with the contract's
// HTTPValidationError list.
func requestValidator(spec *openapi3.T) func(http.Handler) http.Handler {
	// A spec with servers makes the router match on Host; the Host guard owns that.
	spec.Servers = nil
	return nethttpmiddleware.OapiRequestValidatorWithOptions(spec, &nethttpmiddleware.Options{
		SilenceServersWarning: true,
		Options: openapi3filter.Options{
			AuthenticationFunc: openapi3filter.NoopAuthenticationFunc,
			MultiError:         true,
		},
		ErrorHandlerWithOpts: func(_ context.Context, err error, w http.ResponseWriter, _ *http.Request, opts nethttpmiddleware.ErrorHandlerOpts) {
			switch {
			case errors.Is(err, routers.ErrMethodNotAllowed):
				httpx.WriteError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
			case opts.MatchedRoute == nil:
				httpx.WriteError(w, http.StatusNotFound, "Not Found")
			case opts.StatusCode == http.StatusBadRequest:
				writeValidation(w, err)
			default:
				httpx.WriteError(w, http.StatusInternalServerError, "Internal server error")
			}
		},
	})
}

func writeValidation(w http.ResponseWriter, err error) {
	var details []validationDetail
	var multi openapi3.MultiError
	if errors.As(err, &multi) {
		for _, e := range multi {
			details = append(details, detailFor(e))
		}
	} else {
		details = append(details, detailFor(err))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnprocessableEntity)
	// The encode error is a broken client connection; nothing to report.
	_ = json.NewEncoder(w).Encode(map[string]any{"detail": details})
}

func detailFor(err error) validationDetail {
	d := validationDetail{Loc: []string{"request"}, Msg: "Invalid request", Type: "validation_error"}
	var re *openapi3filter.RequestError
	if !errors.As(err, &re) {
		return d
	}
	switch {
	case re.Parameter != nil:
		d.Loc = []string{re.Parameter.In, re.Parameter.Name}
	case re.RequestBody != nil:
		d.Loc = []string{"body"}
	}
	if re.Reason != "" {
		d.Msg = re.Reason
	}
	if len(d.Msg) > maxMessage {
		d.Msg = d.Msg[:maxMessage]
	}
	return d
}
