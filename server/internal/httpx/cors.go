package httpx

import (
	"net/http"

	"github.com/rs/cors"
)

// CORS allows the frontend origin with credentials. In dev it also allows any localhost port;
// a production deployment trusts the frontend alone.
func CORS(frontendOrigin string, dev bool) func(http.Handler) http.Handler {
	opts := cors.Options{
		AllowedOrigins:   []string{frontendOrigin},
		AllowedMethods:   []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"*"},
		AllowCredentials: true,
	}
	if dev {
		opts.AllowOriginFunc = func(origin string) bool {
			return origin == frontendOrigin || localhostOrigin.MatchString(origin)
		}
	}
	return cors.New(opts).Handler
}
