package controller

import (
	"fmt"
	"path"
	"sort"
	"strings"

	nodev1alpha1 "brewlet-operator/api/nodeprofile/v1alpha1"
	"brewlet-operator/internal/brewlet"

	"github.com/distribution/reference"
	"k8s.io/apimachinery/pkg/util/validation"
)

// ValidateNodeProfile enforces the NodeProfile invariants the validating webhook
// rejects early (§8.3): a non-empty JDK list, valid curated or custom sources, a
// valid containerdRestart value, and a positive feature version. The
// cross-profile "two profiles naming the same pool" check is
// ValidateNoPoolConflicts, which needs the sibling set.
func ValidateNodeProfile(profile *nodev1alpha1.NodeProfile) error {
	if len(profile.Spec.JDKs) == 0 {
		return fmt.Errorf("spec.jdks must list at least one JDK")
	}
	tokens := make(map[string]struct{}, len(profile.Spec.JDKs))
	for i, j := range profile.Spec.JDKs {
		if j.Feature <= 0 {
			return fmt.Errorf("spec.jdks[%d].feature must be a positive version, got %d", i, j.Feature)
		}
		if errs := validation.IsDNS1123Label(j.Distribution); len(errs) > 0 {
			return fmt.Errorf("spec.jdks[%d].distribution %q is invalid: %s", i, j.Distribution, strings.Join(errs, "; "))
		}
		const maxDistributionLength = 48
		if len(j.Distribution) > maxDistributionLength {
			return fmt.Errorf("spec.jdks[%d].distribution %q exceeds %d characters", i, j.Distribution, maxDistributionLength)
		}
		token := j.Token()
		const maxJDKTokenLength = validation.DNS1123LabelMaxLength - len("jdk.")
		if len(token) > maxJDKTokenLength {
			return fmt.Errorf("spec.jdks[%d] token %q exceeds %d characters", i, token, maxJDKTokenLength)
		}
		if _, exists := tokens[token]; exists {
			return fmt.Errorf("spec.jdks[%d] duplicates %q", i, token)
		}
		tokens[token] = struct{}{}
		if err := validateJDKSource(i, j); err != nil {
			return err
		}
	}
	switch profile.Spec.Rollout.ContainerdRestart {
	case "", nodev1alpha1.ContainerdRestartValidated,
		nodev1alpha1.ContainerdRestartSIGHUP, nodev1alpha1.ContainerdRestartNone:
	default:
		return fmt.Errorf("spec.rollout.containerdRestart %q is invalid; want one of validated|sighup|none",
			profile.Spec.Rollout.ContainerdRestart)
	}
	return nil
}

func validateJDKSource(index int, j nodev1alpha1.JDKRef) error {
	if isCuratedDistribution(j.Distribution) {
		if j.Source != nil {
			return fmt.Errorf("spec.jdks[%d].source must be omitted for curated distribution %q", index, j.Distribution)
		}
		return nil
	}
	if j.Source == nil {
		return fmt.Errorf("spec.jdks[%d].source is required for non-curated distribution %q", index, j.Distribution)
	}
	if err := validateImageReference(j.Source.Image); err != nil {
		return fmt.Errorf("spec.jdks[%d].source.image: %w", index, err)
	}
	if strings.ContainsAny(j.Source.JavaHome, " \t\r\n") ||
		!path.IsAbs(j.Source.JavaHome) ||
		path.Clean(j.Source.JavaHome) != j.Source.JavaHome ||
		j.Source.JavaHome == "/" {
		return fmt.Errorf("spec.jdks[%d].source.javaHome %q must be a clean absolute path below /", index, j.Source.JavaHome)
	}
	return nil
}

func validateImageReference(ref string) error {
	if ref == "" || strings.ContainsAny(ref, " \t\r\n") || strings.Contains(ref, "://") {
		return fmt.Errorf("%q must be a non-empty OCI reference without whitespace or a URL scheme", ref)
	}
	slash := strings.IndexByte(ref, '/')
	if slash <= 0 {
		return fmt.Errorf("%q must include an explicit registry host", ref)
	}
	host := ref[:slash]
	if host != "localhost" && !strings.ContainsAny(host, ".:") {
		return fmt.Errorf("%q must use a fully qualified registry host", ref)
	}
	if _, err := reference.ParseNormalizedNamed(ref); err != nil {
		return fmt.Errorf("%q is not a valid OCI image reference: %w", ref, err)
	}
	return nil
}

// ValidateNoPoolConflicts rejects a profile that names a pool already claimed by
// another profile (ambiguous ownership — the pool-model replacement for the old
// priority tie-break, §5.6/§8.3). others is the set of existing profiles
// (excluding the one under validation).
func ValidateNoPoolConflicts(profile *nodev1alpha1.NodeProfile, others []nodev1alpha1.NodeProfile) error {
	claimed := map[string]string{} // pool name -> owning profile
	for i := range others {
		o := &others[i]
		if o.Name == profile.Name {
			continue
		}
		for _, name := range o.Spec.NodePool.Names {
			claimed[name] = o.Name
		}
	}
	var dupes []string
	for _, name := range profile.Spec.NodePool.Names {
		if owner, ok := claimed[name]; ok {
			dupes = append(dupes, fmt.Sprintf("%q (already claimed by profile %q)", name, owner))
		}
	}
	if len(dupes) > 0 {
		sort.Strings(dupes)
		return fmt.Errorf("pool ownership conflict: %v", dupes)
	}
	return nil
}

func isCuratedDistribution(dist string) bool {
	for _, d := range brewlet.CuratedDistributions {
		if d == dist {
			return true
		}
	}
	return false
}
