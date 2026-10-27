package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/ahmetb/kubectx/internal/env"
	"github.com/ahmetb/kubectx/internal/kubeconfig"
)

func installFakeKubectl(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake kubectl shell script unsupported on windows")
	}
	dir := filepath.Join(t.TempDir(), "path with spaces")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "tsh-kubectl")
	const script = `#!/bin/sh
# Model tsh's argument parser: kubectl flags cannot precede the subcommand.
case "$1" in
  -*) printf 'unknown long flag: %s\n' "$1" >&2; exit 2 ;;
esac
printf '%s\n' "$@" > "$KUBECTX_TEST_ARGS"
printf '%s' "$KUBECONFIG" > "$KUBECTX_TEST_CONFIG"
printf '%s' "$KUBECTX_TEST_STDERR" >&2
printf '%s' "$KUBECTX_TEST_OUTPUT"
exit "${KUBECTX_TEST_EXIT:-0}"
`
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(env.EnvKubectl, path)
	t.Setenv("_MOCK_NAMESPACES", "")
	t.Setenv("KUBECTX_TEST_ARGS", filepath.Join(dir, "args"))
	t.Setenv("KUBECTX_TEST_CONFIG", filepath.Join(dir, "config"))
	t.Setenv("KUBECTX_TEST_OUTPUT", "")
	t.Setenv("KUBECTX_TEST_STDERR", "")
	t.Setenv("KUBECTX_TEST_EXIT", "0")
	return path
}

func loadKubectlTestConfig(t *testing.T) *kubeconfig.Kubeconfig {
	t.Helper()
	t.Setenv("KUBECONFIG", writeTestKubeconfigWithNamespace(t))
	kc := new(kubeconfig.Kubeconfig).WithLoader(kubeconfig.DefaultLoader)
	t.Cleanup(func() { kc.Close() })
	if err := kc.Parse(); err != nil {
		t.Fatal(err)
	}
	return kc
}

func assertKubectlArgs(t *testing.T, want []string) {
	t.Helper()
	b, err := os.ReadFile(os.Getenv("KUBECTX_TEST_ARGS"))
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("kubectl arguments (-want +got):\n%s", diff)
	}
}

func TestQueryNamespacesKubectl(t *testing.T) {
	installFakeKubectl(t)
	kc := loadKubectlTestConfig(t)
	// The loaded config paths, rather than a subsequently changed environment,
	// must be used. Multiple files and spaces must retain their boundaries.
	paths := append(kc.ConfigPaths(), writeTestKubeconfigWithNamespace(t))
	t.Setenv("KUBECONFIG", strings.Join(paths, string(os.PathListSeparator)))
	if err := kc.Close(); err != nil {
		t.Fatal(err)
	}
	if err := kc.Parse(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", "/wrong/kubeconfig")
	t.Setenv("KUBECTX_TEST_OUTPUT", `{"items":[{"metadata":{"name":"ns1"}},{"metadata":{"name":"ns2"}}]}`)
	t.Setenv("KUBECTX_TEST_STDERR", "MFA authentication required\n")
	var stderr bytes.Buffer
	got, err := queryNamespaces(kc, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"ns1", "ns2"}, got); diff != "" {
		t.Fatalf("namespaces (-want +got):\n%s", diff)
	}
	assertKubectlArgs(t, []string{"get", "--context", "ctx-a", "namespaces", "--output=json"})
	b, err := os.ReadFile(os.Getenv("KUBECTX_TEST_CONFIG"))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != strings.Join(paths, string(os.PathListSeparator)) {
		t.Fatalf("unexpected KUBECONFIG: %q", b)
	}
	if stderr.String() != "MFA authentication required\n" {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestNamespaceExistsKubectl(t *testing.T) {
	for _, tt := range []struct {
		name, output, exit string
		want               bool
		wantErr            string
	}{
		{name: "exists", output: `{"metadata":{"name":"ns2"}}`, want: true},
		{name: "not found", output: "\n"},
		{name: "forbidden", exit: "1", wantErr: "exit status 1"},
		{name: "invalid json", output: "not json", wantErr: "failed to decode namespace"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			installFakeKubectl(t)
			kc := loadKubectlTestConfig(t)
			t.Setenv("KUBECTX_TEST_OUTPUT", tt.output)
			if tt.exit != "" {
				t.Setenv("KUBECTX_TEST_EXIT", tt.exit)
			}
			got, err := namespaceExists(kc, "ns2", io.Discard)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
			} else if err != nil || got != tt.want {
				t.Fatalf("namespaceExists = %v, %v; want %v, nil", got, err, tt.want)
			}
			assertKubectlArgs(t, []string{"get", "--context", "ctx-a", "namespace", "--ignore-not-found", "--output=json", "--", "ns2"})
		})
	}
}

func TestQueryNamespacesKubectlErrors(t *testing.T) {
	for _, tt := range []struct {
		name, output, exit, wantErr string
	}{
		{name: "invalid json", output: "not json", wantErr: "failed to decode namespace list"},
		{name: "command failure", exit: "1", wantErr: "exit status 1"},
		{name: "empty output", wantErr: "failed to decode namespace list"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			installFakeKubectl(t)
			kc := loadKubectlTestConfig(t)
			t.Setenv("KUBECTX_TEST_OUTPUT", tt.output)
			if tt.exit != "" {
				t.Setenv("KUBECTX_TEST_EXIT", tt.exit)
			}
			_, err := queryNamespaces(kc, io.Discard)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error = %v, want %q", err, tt.wantErr)
			}
		})
	}

	t.Run("missing executable", func(t *testing.T) {
		kc := loadKubectlTestConfig(t)
		t.Setenv("_MOCK_NAMESPACES", "")
		t.Setenv(env.EnvKubectl, filepath.Join(t.TempDir(), "missing-kubectl"))
		if _, err := queryNamespaces(kc, io.Discard); err == nil {
			t.Fatal("expected an error rather than falling back to client-go")
		}
	})
}

func isolateNamespaceState(t *testing.T) {
	t.Helper()
	old := defaultDir
	defaultDir = t.TempDir()
	t.Cleanup(func() { defaultDir = old })
}

func TestSwitchNamespaceKubectl(t *testing.T) {
	installFakeKubectl(t)
	kc := loadKubectlTestConfig(t)
	isolateNamespaceState(t)
	for _, target := range []string{"ns2", "-"} {
		want := target
		if target == "-" {
			want = "ns1"
		}
		t.Setenv("KUBECTX_TEST_OUTPUT", `{"metadata":{"name":"`+want+`"}}`)
		if got, err := switchNamespace(kc, target, false, io.Discard); err != nil || got != want {
			t.Fatalf("switchNamespace(%q) = %q, %v", target, got, err)
		}
	}

	// Force must work without launching the configured command at all.
	t.Setenv(env.EnvKubectl, filepath.Join(t.TempDir(), "missing-kubectl"))
	if _, err := switchNamespace(kc, "new-namespace", true, io.Discard); err != nil {
		t.Fatal(err)
	}

	// Failed validation must leave both the config and previous namespace intact.
	installFakeKubectl(t)
	t.Setenv("KUBECTX_TEST_EXIT", "1")
	before, _ := kc.Bytes()
	previous, _ := NewNSFile("ctx-a").Load()
	if _, err := switchNamespace(kc, "forbidden", false, io.Discard); err == nil {
		t.Fatal("expected validation failure")
	}
	after, _ := kc.Bytes()
	if !bytes.Equal(before, after) {
		t.Fatal("failed validation changed kubeconfig")
	}
	if got, err := NewNSFile("ctx-a").Load(); err != nil || got != previous {
		t.Fatalf("previous namespace = %q, %v; want %q", got, err, previous)
	}
}

func TestInteractiveSwitchOpKubectl(t *testing.T) {
	installFakeKubectl(t)
	t.Setenv("KUBECONFIG", writeTestKubeconfigWithNamespace(t))
	isolateNamespaceState(t)
	installKubensFakeFzf(t)
	t.Setenv("KUBECTX_TEST_FZF_STDIN", filepath.Join(t.TempDir(), "fzf-input"))
	t.Setenv("KUBECTX_TEST_FZF_OUT", "ns2")
	// Include both shapes so this fake supports LIST and GET responses.
	t.Setenv("KUBECTX_TEST_OUTPUT", `{"metadata":{"name":"ns2"},"items":[{"metadata":{"name":"ns1"}},{"metadata":{"name":"ns2"}}]}`)
	if err := (InteractiveSwitchOp{}).Run(io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := (CurrentOp{}).Run(&out, io.Discard); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "ns2" {
		t.Fatalf("current namespace = %q", out.String())
	}
}

func TestNamespaceQueriesDefaultClient(t *testing.T) {
	t.Setenv(env.EnvKubectl, "")
	t.Setenv("_MOCK_NAMESPACES", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/namespaces":
			io.WriteString(w, `{"items":[{"metadata":{"name":"ns1"}}]}`)
		case "/api/v1/namespaces/ns1":
			io.WriteString(w, `{"metadata":{"name":"ns1"}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"kind":"Status","apiVersion":"v1","reason":"NotFound","code":404}`)
		}
	}))
	defer server.Close()
	path := writeTestKubeconfigWithNamespace(t)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b = bytes.ReplaceAll(b, []byte("https://example.invalid"), []byte(server.URL))
	if err := os.WriteFile(path, b, 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", path)
	kc := new(kubeconfig.Kubeconfig).WithLoader(kubeconfig.DefaultLoader)
	defer kc.Close()
	if err := kc.Parse(); err != nil {
		t.Fatal(err)
	}
	if got, err := queryNamespaces(kc, io.Discard); err != nil || !cmp.Equal(got, []string{"ns1"}) {
		t.Fatalf("queryNamespaces = %v, %v", got, err)
	}
	for _, ns := range []string{"ns1", "missing"} {
		if got, err := namespaceExists(kc, ns, io.Discard); err != nil || got != (ns == "ns1") {
			t.Fatalf("namespaceExists(%q) = %v, %v", ns, got, err)
		}
	}
}
