# kube-cert-http
An adapter that lets Go's net/http package fetch certificates from kubernetes.
Works great with github.com/PalmStoneGames/kube-cert-manager, or any other tool that will create tls secrets within your kubernetes cluster (even manually)

## Secret format

kube-cert-http picks up all secrets of the type kubernetes.io/tls and will grab the certs from them and make them available for Go to use if it gets a request on that domain.
Additionally, the secrets need to have a "domain" label set in their metadata, which corresponds to the domain that the cert/private key should be used for.

## Usage

Usage is quite simple, assuming kubectl proxy is running and can be connected to on its default port (8001), you can do as follows:

```
package main

import (
	"net/http"
	"github.com/PalmStoneGames/kube-cert-http"
	"log"
)

func main() {
	log.Fatal(kubeCertHTTP.ListenAndServeTLS("", kubeCertHTTP.APIHostKubectlProxy, kubeCertHTTP.DefaultNamespace, http.HandlerFunc(handler)))
}

func handler(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte("hello"))
}
```

## Deployment

Setup a deployment with two pods:

- Your application
- Kubectl proxy

You can do this with a deployment that looks like this:

```
apiVersion: extensions/v1beta1
kind: Deployment
metadata:
  labels:
    app: my-app
  name: my-app
spec:
  replicas: 1
  template:
    metadata:
      labels:
        app: my-app
      name: my-app
    spec:
      containers:
        - name: my-app
          image: my-user/my-app:latest
        - name: kubectl-proxy
          image: palmstonegames/kubectl-proxy:1.3.6
```
