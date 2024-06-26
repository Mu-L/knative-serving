/*
Copyright 2020 The Knative Authors

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

package depcheck

// KnownHeavyDependencies is a list of dependencies that are known to increase the
// binary's size by a lot.
var KnownHeavyDependencies = []string{
	"k8s.io/apimachinery/pkg/api/apitesting/fuzzer",

	// As of 2020/10/27 this adds about 13MB to overall binary size.
	"k8s.io/client-go/kubernetes",
	// As of 2020/10/27 this adds about 7MB to overall binary size.
	"contrib.go.opencensus.io/exporter/stackdriver",
}
