package discovery

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestAuthenticatedDiscovery(t *testing.T) {
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" || r.URL.Path != "/api/v1/pods" ||
			r.URL.Query().Get("labelSelector") != "granite.ai/engine=vllm" ||
			r.URL.Query().Get("fieldSelector") != "status.phase=Running,spec.nodeName=gpu-node" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"items":[{"metadata":{"namespace":"inference","name":"engine","uid":"pod-uid"},"spec":{"nodeName":"gpu-node"},"status":{"podIP":"10.0.0.1","containerStatuses":[{"containerID":"containerd://abc"}]}}]}`))
	}))
	defer s.Close()
	dir := t.TempDir()
	ca, token := filepath.Join(dir, "ca.crt"), filepath.Join(dir, "token")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(token, []byte("test-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	w, err := (Client{URL: s.URL, CAFile: ca, TokenFile: token}).Discover(context.Background(), "granite.ai/engine=vllm", "gpu-node")
	if err != nil {
		t.Fatal(err)
	}
	if len(w) != 1 || w[0].UID != "pod-uid" || w[0].Node != "gpu-node" || len(w[0].ContainerIDs) != 1 {
		t.Fatalf("unexpected inventory: %+v", w)
	}
}
