// go.mod — declares this directory as a Go "module" (a versioned unit of code).
//
// The module path ("subscription-service") is the prefix used for all internal
// imports. When you see `import "subscription-service/internal/models"` later,
// Go resolves it relative to this module path — NOT relative to the file.
// This is different from Node/Python where imports are file-relative.
//
// `go 1.22` declares the minimum Go version. Modern Go is very
// backward-compatible; this mainly gates use of newer language features.
//
// The `require` block lists third-party dependencies. You don't usually edit
// this by hand — running `go get github.com/gin-gonic/gin` adds it for you
// and also updates go.sum (a lockfile with cryptographic hashes).

module subscription-service

go 1.24

require (
	github.com/gin-gonic/gin v1.10.0 // HTTP router/framework
	github.com/lib/pq v1.10.9 // Postgres driver for database/sql
)

require github.com/redis/go-redis/v9 v9.22.0

require (
	github.com/bytedance/sonic v1.11.6 // indirect
	github.com/bytedance/sonic/loader v0.1.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/cloudwego/base64x v0.1.4 // indirect
	github.com/cloudwego/iasm v0.2.0 // indirect
	github.com/gabriel-vasile/mimetype v1.4.3 // indirect
	github.com/gin-contrib/sse v0.1.0 // indirect
	github.com/go-playground/locales v0.14.1 // indirect
	github.com/go-playground/universal-translator v0.18.1 // indirect
	github.com/go-playground/validator/v10 v10.20.0 // indirect
	github.com/goccy/go-json v0.10.2 // indirect
	github.com/json-iterator/go v1.1.12 // indirect
	github.com/klauspost/cpuid/v2 v2.2.10 // indirect
	github.com/leodido/go-urn v1.4.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.2 // indirect
	github.com/pelletier/go-toml/v2 v2.2.2 // indirect
	github.com/twitchyliquid64/golang-asm v0.15.1 // indirect
	github.com/ugorji/go/codec v1.2.12 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	golang.org/x/arch v0.8.0 // indirect
	golang.org/x/crypto v0.23.0 // indirect
	golang.org/x/net v0.25.0 // indirect
	golang.org/x/sys v0.30.0 // indirect
	golang.org/x/text v0.15.0 // indirect
	google.golang.org/protobuf v1.34.1 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)
