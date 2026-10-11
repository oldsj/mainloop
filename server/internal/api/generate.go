// Package api holds code generated from api/openapi.yaml, the wire contract.
package api

//go:generate go tool oapi-codegen -config oapi-codegen.yaml ../../../api/openapi.yaml
//go:generate go run ../../tools/genfallback api.gen.go unimplemented.gen.go
