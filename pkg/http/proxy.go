/*
Copyright 2018 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package http

import (
	"net/http"
	"net/http/httputil"

	netheader "knative.dev/networking/pkg/http/header"
)

// NoHostOverride signifies that no host overriding should be done and that the host
// should be inferred from the target of the reverse-proxy.
const NoHostOverride = ""

// NewHeaderPruningReverseProxy returns a httputil.ReverseProxy that proxies
// requests to the given targetHost after removing the headersToRemove.
// If hostOverride is not an empty string, the outgoing request's Host header will be
// replaced with that explicit value and the passthrough loadbalancing header will be
// set to enable pod-addressability.
func NewHeaderPruningReverseProxy(target, hostOverride string, headersToRemove []string, useHTTPS bool) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			if useHTTPS {
				req.URL.Scheme = "https"
			} else {
				req.URL.Scheme = "http"
			}
			req.URL.Host = target

			if hostOverride != NoHostOverride {
				req.Host = hostOverride
				req.Header.Add(netheader.PassthroughLoadbalancingKey, "true")
			}

			for _, h := range headersToRemove {
				req.Header.Del(h)
			}
		},
	}
}
