// Command brewlet-managed-dependencies is a Ratify (v1.4.x) external verifier
// plugin that admits Kubernetes workloads only when their image carries a valid
// Brewlet managed-dependency attestation signed by a trusted key and naming an
// expected application-builder identity.
//
// The plugin discovers and verifies Brewlet's native OCI 1.1 referrer artifact
// (artifactType application/vnd.brewlet.attestation.v1+json) WITHOUT republishing
// it in cosign or notation format. Ratify's oras referrer store performs registry
// discovery (OCI 1.1 Referrers API), authentication, and blob fetch; this plugin
// runs Brewlet's exact DSSE/in-toto verification (github.com/brewlet/brewlet/pkg/attest)
// over the fetched DSSE envelope.
//
// Protocol: Ratify invokes this binary as a subprocess, setting RATIFY_VERIFIER_*
// environment variables and passing a PluginInputConfig JSON on stdin; the binary
// writes a VerifierResult JSON to stdout. See
// github.com/ratify-project/ratify/pkg/verifier/plugin/skel at tag v1.4.5.
//
// The blank import of the oras referrer store is REQUIRED: the skel entrypoint
// reconstructs the referrer store in-process from the store config on stdin, and
// the store factory only knows "oras" once its init() has registered it.
package main

import (
	"github.com/ratify-project/ratify/pkg/verifier/plugin/skel"

	_ "github.com/ratify-project/ratify/pkg/referrerstore/oras"
)

func main() {
	skel.PluginMain(pluginName, pluginVersion, VerifyReference, []string{pluginVersion})
}
