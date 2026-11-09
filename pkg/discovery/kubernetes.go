// Package discovery is a read-only Kubernetes REST adapter. Runtime decisions do
// not depend on this slow cluster-level loop or require Kubernetes API writes.
package discovery

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type Workload struct {
	Namespace    string   `json:"namespace"`
	Name         string   `json:"name"`
	UID          string   `json:"uid"`
	Node         string   `json:"node"`
	IP           string   `json:"ip"`
	ContainerIDs []string `json:"container_ids"`
}

type Client struct{ URL, TokenFile, CAFile string }

func (c Client) Discover(ctx context.Context, selector, node string) ([]Workload, error) {
	u, err := url.Parse(c.URL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, fmt.Errorf("Kubernetes API URL must use HTTPS")
	}
	token, err := os.ReadFile(c.TokenFile)
	if err != nil {
		return nil, err
	}
	ca, err := os.ReadFile(c.CAFile)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("invalid Kubernetes CA")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	u.Path = "/api/v1/pods"
	q := url.Values{"labelSelector": {selector}, "fieldSelector": {"status.phase=Running"}}
	if node != "" {
		q.Set("fieldSelector", "status.phase=Running,spec.nodeName="+node)
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list Pods: HTTP %d", resp.StatusCode)
	}
	var list struct {
		Items []struct {
			Metadata struct{ Namespace, Name, UID string }
			Spec     struct{ NodeName string }
			Status   struct {
				PodIP             string
				ContainerStatuses []struct{ ContainerID string }
			}
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, err
	}
	out := make([]Workload, 0, len(list.Items))
	for _, pod := range list.Items {
		w := Workload{Namespace: pod.Metadata.Namespace, Name: pod.Metadata.Name, UID: pod.Metadata.UID, Node: pod.Spec.NodeName, IP: pod.Status.PodIP}
		for _, s := range pod.Status.ContainerStatuses {
			if s.ContainerID != "" {
				w.ContainerIDs = append(w.ContainerIDs, s.ContainerID)
			}
		}
		out = append(out, w)
	}
	return out, nil
}
