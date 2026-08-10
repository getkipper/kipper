module github.com/getkipper/kipper/gateway

go 1.25.12

require (
	github.com/getkipper/kipper/controller v0.0.0-00010101000000-000000000000
	github.com/go-chi/chi/v5 v5.3.0
	github.com/go-chi/httprate v0.15.0
)

require (
	github.com/klauspost/cpuid/v2 v2.2.11 // indirect
	github.com/zeebo/xxh3 v1.0.2 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
)

replace github.com/getkipper/kipper/controller => ../controller
