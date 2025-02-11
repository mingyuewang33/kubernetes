/*
Copyright 2014 The Kubernetes Authors.

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

// apiserver is the main api server and master for the cluster.
// it is responsible for serving the cluster management API.
package main

import (
	"os"
	"runtime/debug"
	"strings"

	"k8s.io/apiserver/pkg/server"
	"k8s.io/component-base/cli"
	_ "k8s.io/component-base/logs/json/register"          // for JSON log format registration
	_ "k8s.io/component-base/metrics/prometheus/clientgo" // load all the prometheus client-go plugins
	_ "k8s.io/component-base/metrics/prometheus/version"  // for version metric registration
	"k8s.io/kubernetes/cmd/kube-apiserver/app"
)

func main() {
	if godebug := os.Getenv("GODEBUG"); !strings.Contains(godebug, "x509sha1") {
		// match go1.17 default behavior on release-1.23
		// see https://go.dev/doc/go1.18#sha1
		os.Setenv("GODEBUG", strings.TrimPrefix(godebug+",x509sha1=1", ","))
	}
	if os.Getenv("GOGC") == "" {
		// match go1.17 default GC tuning on release-1.23
		// see https://github.com/kubernetes/kubernetes/issues/108357#issuecomment-1056901991
		debug.SetGCPercent(63)
	}

	command := app.NewAPIServerCommand(server.SetupSignalHandler())
	code := cli.Run(command)
	os.Exit(code)
}
