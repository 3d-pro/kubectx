package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/ahmetb/kubectx/internal/env"
	"github.com/ahmetb/kubectx/internal/kubeconfig"
)

// runKubectl invokes a single executable, not a shell command. Wrappers such as
// tsh-kubectl can add arguments while preserving argument boundaries and stdin
// for interactive authentication. Keep stderr live for login/MFA prompts.
func runKubectl(kc *kubeconfig.Kubeconfig, stderr io.Writer, subcommand string, args ...string) ([]byte, error) {
	ctx, err := kc.GetCurrentContext()
	if err != nil {
		return nil, fmt.Errorf("failed to get current context: %w", err)
	}
	// tsh kubectl starts forwarding arguments after the subcommand. Leading
	// kubectl flags would instead be parsed as tsh flags and rejected.
	args = append([]string{subcommand, "--context", ctx}, args...)
	command := os.Getenv(env.EnvKubectl)
	cmd := exec.Command(command, args...)
	cmd.Stdin = os.Stdin
	cmd.Stderr = stderr
	cmd.Env = os.Environ()
	if paths := kc.ConfigPaths(); len(paths) > 0 {
		cmd.Env = append(cmd.Env, "KUBECONFIG="+strings.Join(paths, string(os.PathListSeparator)))
	}
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%q failed: %w", command, err)
	}
	return out, nil
}

func queryNamespacesWithKubectl(kc *kubeconfig.Kubeconfig, stderr io.Writer) ([]string, error) {
	out, err := runKubectl(kc, stderr, "get", "namespaces", "--output=json")
	if err != nil {
		return nil, err
	}
	var list corev1.NamespaceList
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("failed to decode namespace list from KUBECTL: %w", err)
	}
	names := make([]string, 0, len(list.Items))
	for _, ns := range list.Items {
		names = append(names, ns.Name)
	}
	return names, nil
}

func namespaceExistsWithKubectl(kc *kubeconfig.Kubeconfig, ns string, stderr io.Writer) (bool, error) {
	// Query a single namespace, matching client-go's GET permission requirement.
	// Only NotFound is ignored; authentication and authorization errors still fail.
	out, err := runKubectl(kc, stderr, "get", "namespace", "--ignore-not-found", "--output=json", "--", ns)
	if err != nil {
		return false, err
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return false, nil
	}
	var namespace corev1.Namespace
	if err := json.Unmarshal(out, &namespace); err != nil {
		return false, fmt.Errorf("failed to decode namespace from KUBECTL: %w", err)
	}
	return namespace.Name == ns, nil
}
