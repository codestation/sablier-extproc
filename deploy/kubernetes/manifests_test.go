package kubernetes_test

import (
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestMainManifestContracts(t *testing.T) {
	documents := decodeDocuments(t, "all.yaml")
	extension := findResource(t, documents, "GatewayExtension", "sablier-extproc")
	spec := object(t, extension, "spec")
	if _, found := spec["type"]; found {
		t.Fatal("GatewayExtension must omit deprecated spec.type")
	}
	extProc := object(t, spec, "extProc")
	if value(t, extProc, "failOpen") != false || value(t, extProc, "messageTimeout") != "5s" {
		t.Fatalf("unexpected ext_proc failure settings: %v", extProc)
	}
	mode := object(t, extProc, "processingMode")
	wantMode := map[string]any{
		"requestHeaderMode":   "SEND",
		"responseHeaderMode":  "SKIP",
		"requestBodyMode":     "NONE",
		"responseBodyMode":    "NONE",
		"requestTrailerMode":  "SKIP",
		"responseTrailerMode": "SKIP",
	}
	for field, want := range wantMode {
		if got := value(t, mode, field); got != want {
			t.Fatalf("processingMode.%s = %v; want %v", field, got, want)
		}
	}

	policy := findResource(t, documents, "TrafficPolicy", "sablier-extproc")
	policySpec := object(t, policy, "spec")
	targets := array(t, policySpec, "targetRefs")
	if len(targets) != 1 || value(t, objectAt(t, targets, 0), "kind") != "HTTPRoute" {
		t.Fatalf("TrafficPolicy must target the complete HTTPRoute: %v", targets)
	}

	service := findResource(t, documents, "Service", "sablier-extproc")
	ports := array(t, object(t, service, "spec"), "ports")
	if len(ports) != 1 {
		t.Fatalf("ext_proc Service must expose only gRPC: %v", ports)
	}
	grpcPort := objectAt(t, ports, 0)
	if value(t, grpcPort, "appProtocol") != "kubernetes.io/h2c" {
		t.Fatalf("gRPC Service port lacks h2c appProtocol: %v", grpcPort)
	}

	deployment := findResource(t, documents, "Deployment", "sablier-extproc")
	podSpec := object(t, object(t, object(t, deployment, "spec"), "template"), "spec")
	if value(t, podSpec, "automountServiceAccountToken") != false {
		t.Fatal("ServiceAccount token must not be mounted")
	}
	if value(t, podSpec, "enableServiceLinks") != false {
		t.Fatal("Service environment variable injection must be disabled")
	}
	podSecurity := object(t, podSpec, "securityContext")
	if value(t, podSecurity, "runAsNonRoot") != true ||
		value(t, podSecurity, "runAsUser") != 65532 ||
		value(t, podSecurity, "runAsGroup") != 65532 {
		t.Fatalf("pod must run with the fixed non-root identity: %v", podSecurity)
	}
	container := objectAt(t, array(t, podSpec, "containers"), 0)
	security := object(t, container, "securityContext")
	if value(t, security, "readOnlyRootFilesystem") != true || value(t, security, "allowPrivilegeEscalation") != false {
		t.Fatalf("container security context is incomplete: %v", security)
	}

	namespace := findResource(t, documents, "Namespace", "sablier-extproc")
	labels := object(t, object(t, namespace, "metadata"), "labels")
	if value(t, labels, "pod-security.kubernetes.io/enforce") != "restricted" {
		t.Fatalf("namespace does not enforce restricted Pod Security: %v", labels)
	}

	backend := findResource(t, documents, "Deployment", "app-backend")
	backendPodSpec := object(t, object(t, object(t, backend, "spec"), "template"), "spec")
	backendPodSecurity := object(t, backendPodSpec, "securityContext")
	backendSecurity := object(t, objectAt(t, array(t, backendPodSpec, "containers"), 0), "securityContext")
	if value(t, backendPodSpec, "automountServiceAccountToken") != false ||
		value(t, backendPodSpec, "enableServiceLinks") != false ||
		value(t, backendPodSecurity, "runAsNonRoot") != true ||
		value(t, backendPodSecurity, "runAsUser") != 65532 ||
		value(t, backendPodSecurity, "runAsGroup") != 65532 ||
		value(t, backendSecurity, "readOnlyRootFilesystem") != true ||
		value(t, backendSecurity, "allowPrivilegeEscalation") != false {
		t.Fatalf("example backend security context is incomplete: %v %v", backendPodSpec, backendSecurity)
	}
}

func TestCrossNamespaceExamplesContainAllGrants(t *testing.T) {
	documents := decodeDocuments(t, "cross-namespace-reference-grants.yaml")
	if len(documents) != 3 {
		t.Fatalf("got %d ReferenceGrants; want 3", len(documents))
	}
	for _, document := range documents {
		if value(t, document, "kind") != "ReferenceGrant" {
			t.Fatalf("unexpected cross-namespace resource: %v", document)
		}
	}
}

func TestWorkflowYAML(t *testing.T) {
	documents := decodeDocuments(t, "../../.github/workflows/ci.yaml")
	if len(documents) != 1 {
		t.Fatalf("workflow contains %d YAML documents; want 1", len(documents))
	}
	jobs := object(t, documents[0], "jobs")
	for _, name := range []string{"verify", "envoy-integration", "oci"} {
		if _, found := jobs[name]; !found {
			t.Fatalf("workflow job %q is missing", name)
		}
	}
	for jobName, rawJob := range jobs {
		job, ok := rawJob.(map[string]any)
		if !ok {
			t.Fatalf("workflow job %q is not an object: %v", jobName, rawJob)
		}
		for index, rawStep := range array(t, job, "steps") {
			step, ok := rawStep.(map[string]any)
			if !ok {
				t.Fatalf("workflow job %q step %d is not an object: %v", jobName, index, rawStep)
			}
			uses, found := step["uses"].(string)
			if !found {
				continue
			}
			_, revision, found := strings.Cut(uses, "@")
			if !found || len(revision) != 40 || strings.Trim(revision, "0123456789abcdef") != "" {
				t.Fatalf("workflow job %q step %d must pin uses to a full commit SHA: %q", jobName, index, uses)
			}
		}
	}
}

func decodeDocuments(t *testing.T, path string) []map[string]any {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Errorf("close %s: %v", path, err)
		}
	}()

	decoder := yaml.NewDecoder(file)
	var documents []map[string]any
	for {
		var document map[string]any
		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		if len(document) > 0 {
			documents = append(documents, document)
		}
	}
	return documents
}

func objectAt(t *testing.T, values []any, index int) map[string]any {
	t.Helper()
	if index >= len(values) {
		t.Fatalf("index %d is missing from %v", index, values)
	}
	object, ok := values[index].(map[string]any)
	if !ok {
		t.Fatalf("index %d is not an object in %v", index, values)
	}
	return object
}

func findResource(t *testing.T, documents []map[string]any, kind, name string) map[string]any {
	t.Helper()
	for _, document := range documents {
		metadata := object(t, document, "metadata")
		if value(t, document, "kind") == kind && value(t, metadata, "name") == name {
			return document
		}
	}
	t.Fatalf("resource %s/%s not found", kind, name)
	return nil
}

func object(t *testing.T, parent map[string]any, key string) map[string]any {
	t.Helper()
	object, ok := parent[key].(map[string]any)
	if !ok {
		t.Fatalf("%s is not an object in %v", key, parent)
	}
	return object
}

func array(t *testing.T, parent map[string]any, key string) []any {
	t.Helper()
	array, ok := parent[key].([]any)
	if !ok {
		t.Fatalf("%s is not an array in %v", key, parent)
	}
	return array
}

func value(t *testing.T, parent map[string]any, key string) any {
	t.Helper()
	value, ok := parent[key]
	if !ok {
		t.Fatalf("%s is missing from %v", key, parent)
	}
	return value
}
