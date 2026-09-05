package webapi

import (
	"embed"
	"io/fs"
	"net/http"

	"github.com/gin-gonic/gin"
)

//go:embed static
var staticFiles embed.FS

// mountStatic serves the embedded dashboard at "/", via NoRoute rather than
// gin's StaticFS: registering a "/*filepath" wildcard route at the root
// panics as soon as any other top-level route (like "/api") already exists,
// since gin's route tree cannot combine the two. NoRoute has no such
// conflict, since it only ever runs for a request nothing else matched.
func mountStatic(r *gin.Engine) {
	sub, err := fs.Sub(staticFiles, "static")
	if err != nil {
		// The embed directive guarantees "static" exists at build time.
		panic(err)
	}
	r.NoRoute(gin.WrapH(http.FileServerFS(sub)))
}
