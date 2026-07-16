// generate-rules writes the Prometheus rule YAML files derived from the typed
// alert definitions in pkg/monitoring/rules/alerts. Run via `make generate`.
//
// Flags:
//
//	-namespace <ns>   Kubernetes namespace embedded in the PrometheusRule manifest.
//	                  Falls back to the NAMESPACE environment variable.
//	                  If neither is set, the namespace field is omitted from the
//	                  manifest (deployers set it at apply time).
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/csi-addons/csi-volume-device-exporter/pkg/monitoring/rules"
)

func main() {
	namespace := flag.String("namespace", "", "Kubernetes namespace for the PrometheusRule manifest (overrides NAMESPACE env var)")
	flag.Parse()

	if *namespace == "" {
		*namespace = os.Getenv("NAMESPACE")
	}

	// Resolve the repository root relative to this file so the tool works
	// regardless of the working directory when invoked via `go run`.
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")
	rulesDir := filepath.Join(repoRoot, "pkg", "monitoring", "rules")

	if err := rules.WritePrometheusRuleManifest(filepath.Join(rulesDir, "alerts.yaml"), *namespace); err != nil {
		fmt.Fprintf(os.Stderr, "generate-rules: %v\n", err)
		os.Exit(1)
	}
	if *namespace == "" {
		fmt.Println("wrote pkg/monitoring/rules/alerts.yaml (namespace: omitted, set at deploy time)")
	} else {
		fmt.Printf("wrote pkg/monitoring/rules/alerts.yaml (namespace: %s)\n", *namespace)
	}
}
