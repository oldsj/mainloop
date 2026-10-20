// Command kagentproto generates the Go and Connect clients for the vendored kagent protos under
// server/third_party/kagentproto/kagent/api/v1alpha1, without buf or network access: it parses and
// links the proto sources with protocompile, resolving buf/validate/validate.proto from the file
// descriptor already registered by the vendored protovalidate Go package (see the blank import
// below) and the Google well-known types from protocompile's bundled standard imports, then drives
// protoc-gen-go and protoc-gen-connect-go (both `go tool` entries in server/go.mod) through the
// normal protoc plugin stdin/stdout protocol.
//
//	go run ./tools/kagentproto -root <dir containing kagent/api/v1alpha1/*.proto>
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"

	"github.com/bufbuild/protocompile"
	_ "buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go/buf/validate"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"
)

// protoFiles are the logical import paths of the vendored kagent protos, relative to -root, in the
// same spelling their own import statements use.
var protoFiles = []string{
	"kagent/api/v1alpha1/common.proto",
	"kagent/api/v1alpha1/runtime.proto",
	"kagent/api/v1alpha1/sessions.proto",
	"kagent/api/v1alpha1/agents.proto",
	"kagent/api/v1alpha1/scheduled_runs.proto",
	"kagent/api/v1alpha1/checkpoints.proto",
}

// kagentGoImportPath is where this repository vendors the generated kagent types, which protoc-gen-go
// treats as the same Go package for all of protoFiles (their shared go_package option already makes
// them one package for same-run generation), but which protoc-gen-connect-go's separate "connect"
// subpackage must import by path: the fork's own go_package option names its own module, so each file
// needs an explicit M override to this repository's vendored path instead.
const kagentGoImportPath = "github.com/oldsj/mainloop/server/third_party/kagentproto/kagent/api/v1alpha1"

// goPackageMap tells protoc-gen-go and protoc-gen-connect-go which Go package already carries
// buf/validate/validate.proto's generated types, so the kagent files' field-option extensions
// resolve to that existing module instead of regenerating it, and which Go package the kagent files
// themselves live at in this repository rather than the fork's own module.
var goPackageMap = "Mbuf/validate/validate.proto=buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go/buf/validate" +
	mapEachTo(protoFiles, kagentGoImportPath)

func mapEachTo(files []string, path string) string {
	var b string
	for _, f := range files {
		b += ",M" + f + "=" + path
	}
	return b
}

func main() {
	root := flag.String("root", "", "directory containing kagent/api/v1alpha1/*.proto")
	flag.Parse()
	if *root == "" {
		fmt.Fprintln(os.Stderr, "kagentproto: -root is required")
		os.Exit(2)
	}
	if err := run(*root); err != nil {
		fmt.Fprintln(os.Stderr, "kagentproto:", err)
		os.Exit(1)
	}
}

func run(root string) error {
	req, err := buildRequest(root)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	reqBytes, err := proto.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	for _, tool := range []string{"protoc-gen-go", "protoc-gen-connect-go"} {
		if err := runPlugin(tool, reqBytes, root); err != nil {
			return fmt.Errorf("%s: %w", tool, err)
		}
	}
	return nil
}

// buildRequest compiles the six kagent protos and assembles the CodeGeneratorRequest both plugins
// read: the files to generate, plus every file they import (including buf/validate and the Google
// well-known types) in dependency order, exactly as protoc itself would supply it.
func buildRequest(root string) (*pluginpb.CodeGeneratorRequest, error) {
	resolver := protocompile.WithStandardImports(protocompile.CompositeResolver{
		&protocompile.SourceResolver{ImportPaths: []string{root}},
		protocompile.ResolverFunc(resolveFromGlobalFiles),
	})
	compiler := protocompile.Compiler{Resolver: resolver, SourceInfoMode: protocompile.SourceInfoStandard}
	files, err := compiler.Compile(context.Background(), protoFiles...)
	if err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	var order []*descriptorpb.FileDescriptorProto
	var collect func(protoreflect.FileDescriptor)
	collect = func(fd protoreflect.FileDescriptor) {
		if seen[fd.Path()] {
			return
		}
		seen[fd.Path()] = true
		imps := fd.Imports()
		for i := range imps.Len() {
			collect(imps.Get(i).FileDescriptor)
		}
		order = append(order, protodesc.ToFileDescriptorProto(fd))
	}
	for _, f := range files {
		collect(f)
	}

	param := "paths=source_relative," + goPackageMap
	return &pluginpb.CodeGeneratorRequest{
		FileToGenerate: protoFiles,
		Parameter:      &param,
		ProtoFile:      order,
	}, nil
}

// resolveFromGlobalFiles serves buf/validate/validate.proto (and nothing else) from the descriptor
// the vendored protovalidate Go package registers at init, via its blank import above.
func resolveFromGlobalFiles(path string) (protocompile.SearchResult, error) {
	fd, err := protoregistry.GlobalFiles.FindFileByPath(path)
	if err != nil {
		return protocompile.SearchResult{}, err
	}
	return protocompile.SearchResult{Desc: fd}, nil
}

// runPlugin runs a `go tool` plugin over the protoc plugin stdin/stdout protocol and writes every
// file its response names, relative to root.
func runPlugin(tool string, reqBytes []byte, root string) error {
	cmd := exec.CommandContext(context.Background(), "go", "tool", tool) //nolint:gosec // tool is one of two fixed literals above
	cmd.Stdin = bytes.NewReader(reqBytes)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, stderr.String())
	}
	var resp pluginpb.CodeGeneratorResponse
	if err := proto.Unmarshal(stdout.Bytes(), &resp); err != nil {
		return fmt.Errorf("unmarshal response: %w", err)
	}
	if resp.GetError() != "" {
		return fmt.Errorf("plugin reported: %s", resp.GetError())
	}
	sort.Slice(resp.File, func(i, j int) bool { return resp.File[i].GetName() < resp.File[j].GetName() })
	for _, f := range resp.File {
		path := filepath.Join(root, filepath.FromSlash(f.GetName())) //nolint:gosec // response names are plugin-controlled, not user input
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(f.GetContent()), 0o644); err != nil { //nolint:gosec // generated source, not a secret
			return err
		}
	}
	return nil
}
