// kubecerthttp provides the needed adapters to easily fetch certificates from kubernetes secrets and use them to serve http/1.1, http/2, or any other protocol compatible with tls.Config.
// The expected format of the secrets within kubernetes is the standard kubernetes.io/tls format, which is a secret with a tls.crt and tls.key.
package kubecerthttp

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

const (
	// secretsEndpoint is the path to fetch kubernetes secrets
	secretsWatchEndpoint = "%s/api/v1/namespaces/%s/secrets?watch=true&resourceVersion=%s"

	// APIHostKubectlProxy is the typical API host to use when using kubectl proxy in the pod
	APIHostKubectlProxy = "http://127.0.0.1:8001"
	// DefaultNamespace is the default kubernetes namespace
	DefaultNamespace = "default"
)

// secret is used to deserialize k8s secrets from JSON
type secret struct {
	Kind       string                 `json:"kind"`
	ApiVersion string                 `json:"apiVersion"`
	Metadata   map[string]interface{} `json:"metadata"`
	Data       map[string][]byte      `json:"data"`
	Type       string                 `json:"type"`
}

type secretEvent struct {
	Type   string `json:"type"`
	Object secret `json:"object"`
}

// NewTLSConfig returns a TLS config that will fetch tls certificates from kubernetes secrets with the given prefix.
// By default, the tls.Config is configured to work with http/1.1 and http/2.
// apiHost is the endpoint at which we can connect to kubernetes, usually this is 127.0.0.1:8001 when using kubectl proxy, which is exposed in the constant ApiHostKubectlProxy.
// namespace is the kubernetes namespace to use, to use the default namespace, use the DefaultNamespace constant
// hosts is the hosts to actually fetch certificates for, if left empty all hosts for which certs can be found for will be used
func NewTLSConfig(apiHost, namespace string, hosts ...string) *tls.Config {
	// Bookkeeping variables
	certMap := make(map[string]*tls.Certificate)
	mutex := new(sync.RWMutex)
	tlsCfg := new(tls.Config)

	// GetCertificate
	tlsCfg.GetCertificate = func(clientHello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		mutex.RLock()
		cert := certMap[clientHello.ServerName]
		mutex.RUnlock()

		return cert, nil
	}

	tlsCfg.NextProtos = []string{"h2", "http/1.1"}

	// Monitor routine
	startMonitor(apiHost, namespace, certMap, mutex, hosts)

	return tlsCfg
}

// ListenAndServe directly starts a http and http/2 server
// apiHost is the endpoint at which we can connect to kubernetes, usually this is 127.0.0.1:8001 when using kubectl proxy, which is exposed in the constant ApiHostKubectlProxy.
// namespace is the kubernetes namespace to use, to use the default namespace, use the DefaultNamespace constant
// handler is the http handler to call
// hosts is the hosts to actually fetch certificates for, if left empty all hosts for which certs can be found for will be used
func ListenAndServeTLS(addr string, apiHost, namespace string, handler http.Handler, hosts ...string) error {
	srv := &http.Server{Addr: addr, Handler: handler, TLSConfig: NewTLSConfig(apiHost, namespace)}
	return srv.ListenAndServeTLS("", "")
}

func startMonitor(apiHost, namespace string, certMap map[string]*tls.Certificate, mutex *sync.RWMutex, hosts []string) {
	// convert hosts to a map for convenience
	var hostMap map[string]struct{}
	if hosts != nil {
		hostMap = make(map[string]struct{})
		for _, host := range hosts {
			hostMap[host] = struct{}{}
		}
	}

	go func() {
		c, errC := monitorSecretEvents(apiHost, namespace)
		for {
			select {
			case event := <-c:

				// Skip everything except TLS secrets
				if event.Object.Type != "kubernetes.io/tls" {
					continue
				}

				// Grab the secret name
				secretName, ok := event.Object.Metadata["name"].(string)
				if !ok {
					log.Printf("Secret has no valid name") // Shouldn't happen
					continue
				}

				// Grab the domain name from the labels
				labels, ok := event.Object.Metadata["labels"].(map[string]interface{})
				if !ok {
					log.Printf("Ignoring secret %v due to missing label 'domain'", secretName)
					continue
				}

				domain, ok := labels["domain"].(string)
				if !ok {
					log.Printf("Ignoring secret %v due to missing label 'domain'", secretName)
					continue
				}

				switch event.Type {
				case "ADDED", "MODIFIED":
					if hostMap != nil {
						if _, ok := hostMap[domain]; !ok {
							log.Printf("[%v] Skipping domain", domain)
							continue
						}
					}
					tlsCert, err := parseCert(domain, secretName, &event.Object)
					if err != nil {
						log.Printf("[%v] Error while parsing TLS cert: %v", domain, err)
						continue
					}

					mutex.Lock()
					_, isExisting := certMap[domain]
					certMap[domain] = &tlsCert
					mutex.Unlock()

					if !isExisting {
						log.Printf("[%v] Added certificiate data", domain)
					} else if event.Type == "MODIFIED" {
						log.Printf("[%v] Updated certificate data", domain)
					}
				case "DELETED":
					mutex.Lock()
					_, exists := certMap[domain]
					delete(certMap, domain)
					mutex.Unlock()

					if exists {
						log.Printf("[%v] Removed certificate data", domain)
					}
				}
			case err := <-errC:
				log.Printf("Error while monitoring kubernetes secrets for SSL certs: %v", err)
			}
		}
	}()
}

func monitorSecretEvents(apiHost, namespace string) (<-chan secretEvent, <-chan error) {
	events := make(chan secretEvent)
	errc := make(chan error, 1)
	go func() {
		resourceVersion := "0"
		watch := func() error {
			resp, err := http.Get(fmt.Sprintf(secretsWatchEndpoint, apiHost, namespace, resourceVersion))
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				return errors.New("Invalid status code: " + resp.Status)
			}

			decoder := json.NewDecoder(resp.Body)
			for {
				var event secretEvent
				err = decoder.Decode(&event)
				if err != nil {
					if err != io.EOF {
						return err
					}
					break
				}
				if s, ok := event.Object.Metadata["resourceVersion"].(string); ok {
					resourceVersion = s
				}
				events <- event
			}
			return nil
		}
		for {
			if err := watch(); err != nil {
				errc <- err
			}
			time.Sleep(5 * time.Second)
		}
	}()

	return events, errc
}

func parseCert(domain string, secretName string, secret *secret) (tls.Certificate, error) {
	// Grab data from the secret
	rawCert, ok := secret.Data["tls.crt"]
	if !ok {
		return tls.Certificate{}, fmt.Errorf("Kubernetes secret '%v' does not contain tls.crt", secretName)
	}

	rawKey, ok := secret.Data["tls.key"]
	if !ok {
		return tls.Certificate{}, fmt.Errorf("Kubernetes secret '%v' does not contain tls.key for domain %v", secretName)
	}

	return tls.X509KeyPair(rawCert, rawKey)
}
