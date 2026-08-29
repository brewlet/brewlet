// brewlet is a Phase-0 PoC CLI proving the Brewlet model: a developer ships
// ONLY a JAR (as an OCI artifact); the node-resident JVM runs it with java -jar.
//
//	brewlet push    <jar> <ref> [flags]   publish a JAR as an OCI artifact
//	brewlet inspect <ref>                 show the artifact manifest + config
//	brewlet run     <ref> [flags]         pull + launch java -jar on this node
//	brewlet bundle  <ref> [flags]         emit an OCI runc bundle (shim path)
//	brewlet dependency-bundle <tar> <ref> publish an approved dependency bundle
//	brewlet jdks    [flags]               list JDKs available across the cluster
package main

import (
	"crypto/ecdsa"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/brewlet/brewlet/internal/artifact"
	"github.com/brewlet/brewlet/internal/doctor"
	"github.com/brewlet/brewlet/internal/inventory"
	"github.com/brewlet/brewlet/internal/runtime"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "push":
		err = cmdPush(os.Args[2:])
	case "inspect":
		err = cmdInspect(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:])
	case "bundle":
		err = cmdBundle(os.Args[2:])
	case "dependency-bundle":
		err = cmdDependencyBundle(os.Args[2:])
	case "keygen":
		err = cmdKeygen(os.Args[2:])
	case "jdks":
		err = cmdJDKs(os.Args[2:])
	case "doctor":
		err = cmdDoctor(os.Args[2:])
	case "version", "--version":
		fmt.Println(version)
		return
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`Brewlet PoC — the JVM analogue to SpinKube

USAGE:
  brewlet push    <jar> <ref> [--format image|artifact] [--store DIR] [--config FILE] [--arch amd64,arm64] [--no-arch] [--classpath-layer TAR ...] [--dependency-bundle REF --dependency-lock FILE --trusted-public-key PEM --trusted-signer-identity IDENTITY --signing-key PEM --builder-identity IDENTITY] [--main-class CLASS] [--module-layer TAR ...] [--appcds-archive JSA]
  brewlet dependency-bundle <classpath-tar> <ref> --name NAME --version VERSION --source-bom G:A:V --lock FILE --signing-key PEM --signer-identity IDENTITY [--compatible-jdks 21,25] [--store DIR]
  brewlet keygen --private FILE --public FILE
  brewlet inspect <ref>       [--store DIR] [--trusted-public-key PEM --trusted-signer-identity IDENTITY]
  brewlet run     <ref>       [--store DIR] [--jdk-root DIR] [--launcher NAME] [-- <extra jvm args>]
  brewlet bundle  <ref>       [--store DIR] [--cpu N] [--memory M] [--jdk-root DIR] [--launcher NAME] [--launcher-root DIR] [--out DIR]
  brewlet jdks                [--output table|wide|json] [--kubeconfig FILE] [--context CTX] [--selector SEL]
  brewlet doctor              [--namespace NS] [--output table|json] [--kubeconfig FILE] [--context CTX]
  brewlet version

  <ref> is name:tag, e.g. demo/hello:1.0.0
`)
}

func cmdKeygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	privatePath := fs.String("private", "", "output PKCS#8 PEM private key")
	publicPath := fs.String("public", "", "output SubjectPublicKeyInfo PEM public key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *privatePath == "" || *publicPath == "" {
		return fmt.Errorf("usage: keygen --private FILE --public FILE")
	}
	if err := artifact.GenerateECDSAKeyPair(*privatePath, *publicPath); err != nil {
		return err
	}
	fmt.Printf("generated ECDSA P-256 signing key pair\n  private: %s\n  public: %s\n", *privatePath, *publicPath)
	return nil
}

// splitDoubleDash separates everything after a literal "--" (extra JVM args).
func splitDoubleDash(args []string) (head, tail []string) {
	for i, a := range args {
		if a == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}

// parseInterspersed lets flags appear before AND after positional args
// (Go's flag package otherwise stops at the first positional).
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for len(args) > 0 {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			break
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
	return positional, nil
}

// stringSlice is a repeatable string flag (e.g. --classpath-layer a --classpath-layer b).
type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func cmdPush(args []string) error {
	fs := flag.NewFlagSet("push", flag.ExitOnError)
	store := fs.String("store", "./oci", "OCI layout directory")
	cfgFile := fs.String("config", "", "optional jvm-config.json to embed")
	archFlag := fs.String("arch", "", "constrain scheduling to these architectures for a NON-portable (JNI) JAR, comma-separated (amd64,arm64); overrides auto-detection. Omit for arch-neutral (default)")
	noArch := fs.Bool("no-arch", false, "disable native-library auto-detection; publish with no arch constraint (arch-neutral)")
	var cpLayers stringSlice
	fs.Var(&cpLayers, "classpath-layer", "optional dependency-layer tar to attach (repeatable); see https://github.com/brewlet/site/blob/main/docs/layered-classpath-deployment.md")
	dependencyBundle := fs.String("dependency-bundle", "", "approved managed dependency bundle ref in the same OCI layout; mutually exclusive with --classpath-layer/--module-layer")
	dependencyLock := fs.String("dependency-lock", "", "canonical lock for the application's resolved Maven runtime graph; required with --dependency-bundle")
	trustedPublicKey := fs.String("trusted-public-key", "", "PEM ECDSA P-256 public key trusted to sign the selected bundle")
	trustedSignerIdentity := fs.String("trusted-signer-identity", "", "expected identity in the signed bundle provenance")
	signingKey := fs.String("signing-key", "", "PEM PKCS#8 ECDSA P-256 key used to sign final-image managed-dependency evidence")
	builderIdentity := fs.String("builder-identity", "", "final-image builder identity recorded in signed evidence")
	mainClass := fs.String("main-class", "", "application main class; required with --dependency-bundle unless supplied by a classpath-mode --config")
	var mpLayers stringSlice
	fs.Var(&mpLayers, "module-layer", "optional library-module tar for a modular (JPMS) app, unpacked to /app/mods (repeatable); see https://github.com/brewlet/site/blob/main/docs/jpms-support.md")
	cdsArchive := fs.String("appcds-archive", "", "optional prebuilt AppCDS archive (.jsa) to ship; mounted at /app/<name> and launched with -Xshare:auto -XX:SharedArchiveFile; see https://github.com/brewlet/site/blob/main/docs/appcds.md")
	appcds := fs.Bool("appcds", false, "generate an AppCDS archive by running a self-terminating training JVM against the JAR, then ship it (turnkey equivalent of --appcds-archive); fat-JAR only. See https://github.com/brewlet/site/blob/main/docs/appcds.md §4.2")
	appcdsJava := fs.String("appcds-java", "", "java executable (or JAVA_HOME dir) for --appcds training; defaults to $JAVA_HOME/bin/java, else java on PATH")
	appcdsTimeout := fs.Int("appcds-timeout", 120, "seconds to wait for the --appcds training JVM to self-terminate")
	format := fs.String("format", "image", "delivery format: \"image\" (default; a standard, kubelet-pullable OCI image — a runtimeClassName: brewlet pod can set image: <ref> and containerd/kubelet pull+unpack it as SpinKube does for a Spin-compatible Wasm application) or \"artifact\" (native Brewlet OCI artifact, custom media types — registry-native, delivered to nodes out of band). See https://github.com/brewlet/site/blob/main/docs/runnable-image.md")
	var appcdsArgs stringSlice
	fs.Var(&appcdsArgs, "appcds-arg", "workload argument passed to the --appcds training JVM to drive class loading (repeatable)")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 2 {
		return fmt.Errorf("usage: push <jar> <ref>")
	}
	jarPath, ref := pos[0], pos[1]

	explicitArch := splitArchFlag(*archFlag)

	var cfg artifact.JVMConfig
	if *cfgFile != "" {
		b, err := os.ReadFile(*cfgFile)
		if err != nil {
			return err
		}
		cfg, err = artifact.DecodeConfig(b)
		if err != nil {
			return err
		}
	} else {
		// Auto-detect a modular (JPMS) JAR: a root module-info.class means the
		// artifact is best launched on the module path (`java -p … -m …`).
		entry := artifact.Entry{Mode: "jar"}
		mi, modular, err := artifact.InspectModuleJar(jarPath)
		if err != nil {
			return err
		}
		if modular {
			entry = artifact.Entry{Mode: "module", Module: mi.Name, MainClass: mi.MainClass}
			// When library-module layers are attached, default the module path to
			// the app JAR plus the /app/mods directory they unpack to.
			if len(mpLayers) > 0 {
				entry.ModulePath = []string{filepath.Base(jarPath), "mods"}
			}
			// A modular app may also carry non-modular / automatic-module libraries
			// on the class path: attaching classpath layers yields the mixed form
			// (`java -cp /app/lib/* -p … -m …`). See https://github.com/brewlet/site/blob/main/docs/layered-classpath-deployment.md §8.
			if len(cpLayers) > 0 {
				entry.ClassPath = []string{"lib/*"}
			}
		}
		cfg = artifact.JVMConfig{
			SchemaVersion: 1,
			MainJar:       filepath.Base(jarPath),
			Entry:         entry,
		}
	}

	var managedBundle *artifact.ResolvedDependencyBundle
	var managedSupply artifact.VerifiedBundleSupplyChain
	var managedEvidence *artifact.ManagedDependencyEvidence
	var managedSigningKey *ecdsa.PrivateKey
	if *dependencyBundle != "" {
		if *format != "" && *format != "image" {
			return fmt.Errorf("--dependency-bundle requires --format=image so its standard OCI layer can be reused unchanged")
		}
		if len(cpLayers) > 0 || len(mpLayers) > 0 {
			return fmt.Errorf("--dependency-bundle is mutually exclusive with --classpath-layer and --module-layer")
		}
		if *appcds || *cdsArchive != "" {
			return fmt.Errorf("--dependency-bundle does not support AppCDS in the MVP")
		}
		if err := artifact.ValidateThinJar(jarPath); err != nil {
			return err
		}
		bundle, err := (artifact.Store{Root: *store}).ResolveDependencyBundle(*dependencyBundle)
		if err != nil {
			return fmt.Errorf("resolve --dependency-bundle: %w", err)
		}
		if strings.TrimSpace(*dependencyLock) == "" {
			return fmt.Errorf("--dependency-bundle requires --dependency-lock for the application's resolved Maven runtime graph")
		}
		lockRaw, err := os.ReadFile(*dependencyLock)
		if err != nil {
			return fmt.Errorf("read --dependency-lock: %w", err)
		}
		applicationLock, err := artifact.DecodeDependencyLock(lockRaw)
		if err != nil {
			return fmt.Errorf("decode --dependency-lock: %w", err)
		}
		if err := artifact.VerifyDependencyLock(bundle.Lock, applicationLock); err != nil {
			return err
		}
		if strings.TrimSpace(*trustedPublicKey) == "" || strings.TrimSpace(*trustedSignerIdentity) == "" ||
			strings.TrimSpace(*signingKey) == "" || strings.TrimSpace(*builderIdentity) == "" {
			return fmt.Errorf("--dependency-bundle requires --trusted-public-key, --trusted-signer-identity, --signing-key, and --builder-identity")
		}
		trustedKey, err := artifact.LoadECDSAPublicKey(*trustedPublicKey)
		if err != nil {
			return err
		}
		managedSupply, err = (artifact.Store{Root: *store}).VerifyBundleSupplyChain(bundle, trustedKey, *trustedSignerIdentity)
		if err != nil {
			return fmt.Errorf("verify managed dependency bundle trust: %w", err)
		}
		managedSigningKey, err = artifact.LoadECDSAPrivateKey(*signingKey)
		if err != nil {
			return err
		}
		entryMain := strings.TrimSpace(*mainClass)
		if entryMain == "" && cfg.Entry.Mode == "classpath" {
			entryMain = cfg.Entry.MainClass
		}
		if entryMain == "" {
			return fmt.Errorf("--dependency-bundle requires --main-class (or entry.mainClass in a classpath-mode --config)")
		}
		if cfg.MainJar == "" {
			cfg.MainJar = filepath.Base(jarPath)
		}
		cfg.Entry = artifact.Entry{
			Mode:      "classpath",
			MainClass: entryMain,
			ClassPath: []string{cfg.MainJar, "lib/*"},
		}
		managedBundle = &bundle
		evidence, err := bundle.Evidence(jarPath)
		if err != nil {
			return err
		}
		evidence.SBOMDigest = managedSupply.SBOMDigest
		evidence.BuilderIdentity = *builderIdentity
		managedEvidence = &evidence
	}

	// Resolve the optional arch constraint (§ https://github.com/brewlet/site/blob/main/docs/multi-arch.md). An explicit
	// --arch always wins; --no-arch forces arch-neutral; otherwise scan the JAR
	// for bundled natives and default the constraint for non-portable artifacts.
	switch {
	case len(explicitArch) > 0:
		cfg.Arch = explicitArch
	case *noArch:
		cfg.Arch = nil
	default:
		scan, err := artifact.ScanNativeArch(jarPath)
		if err != nil {
			return err
		}
		if len(scan.Arches) > 0 {
			cfg.Arch = scan.Arches
			fmt.Printf("  detected bundled native libraries -> arch constraint %v (override with --arch, disable with --no-arch)\n", scan.Arches)
		} else if len(scan.Unrecognized) > 0 {
			fmt.Printf("  warning: %d bundled native library(ies) found but their architecture could not be inferred; publishing arch-neutral. Set --arch explicitly if this JAR is not portable (e.g. %s)\n", len(scan.Unrecognized), scan.Unrecognized[0])
		}
	}

	// --appcds turnkey generation (https://github.com/brewlet/site/blob/main/docs/appcds.md §4.2): run a self-terminating
	// training JVM against the fat JAR to produce a .jsa, then treat it exactly
	// like a prebuilt --appcds-archive below. Mutually exclusive with a prebuilt
	// archive and with layered/module layers (fat-JAR only).
	if *appcds {
		if *cdsArchive != "" {
			return fmt.Errorf("--appcds and --appcds-archive are mutually exclusive: --appcds generates the archive, --appcds-archive ships a prebuilt one")
		}
		if len(cpLayers) > 0 || len(mpLayers) > 0 {
			return fmt.Errorf("--appcds supports fat-JAR only; drop --classpath-layer/--module-layer or use the Maven brewlet:appcds goal for layered/module training")
		}
		javaBin, err := resolveJavaBinary(*appcdsJava)
		if err != nil {
			return err
		}
		genDir, err := os.MkdirTemp("", "brewlet-appcds-out-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(genDir)
		archiveName := strings.TrimSuffix(filepath.Base(jarPath), ".jar") + ".jsa"
		genArchive := filepath.Join(genDir, archiveName)
		fmt.Printf("  training AppCDS archive with %s (timeout %ds)...\n", javaBin, *appcdsTimeout)
		if err := runtime.GenerateAppCDSArchive(cfg, jarPath, javaBin, genArchive, time.Duration(*appcdsTimeout)*time.Second, appcdsArgs); err != nil {
			return fmt.Errorf("--appcds: %w", err)
		}
		*cdsArchive = genArchive
	}

	// Wire the optional AppCDS archive (https://github.com/brewlet/site/blob/main/docs/appcds.md). When --appcds-archive is
	// given, default the launch-config cds hint from the file's basename unless a
	// --config already set one; PushWithCDS then enforces they agree.
	if *cdsArchive != "" {
		base := filepath.Base(*cdsArchive)
		if cfg.CDS == nil {
			cfg.CDS = &artifact.CDS{Archive: base, Mode: "dynamic"}
		} else if cfg.CDS.Archive == "" {
			cfg.CDS.Archive = base
		}
	}

	s := artifact.Store{Root: *store}
	switch *format {
	case "artifact":
		desc, err := s.PushWithCDS(ref, cfg, jarPath, cpLayers, mpLayers, *cdsArchive)
		if err != nil {
			return err
		}
		fmt.Printf("pushed %s\n  manifest: %s (%d bytes)\n  artifactType: %s\n  store: %s\n",
			ref, desc.Digest, desc.Size, artifact.ArtifactType, *store)
	case "image", "":
		desc, err := s.PushRunnableImageWithOptions(ref, cfg, jarPath, cpLayers, mpLayers, *cdsArchive, artifact.RunnableImageOptions{
			ManagedDependency: managedBundle,
			ManagedEvidence:   managedEvidence,
		})
		if err != nil {
			return err
		}
		if managedBundle != nil {
			predicate := artifact.ManagedDependencyPredicate{
				SchemaVersion: 1, FinalImageDigest: desc.Digest, ThinJar: true,
				ApplicationJarDigest:   managedEvidence.ApplicationJarDigest,
				DependencyBundleDigest: managedBundle.ManifestDigest,
				DependencyLayerDigest:  managedBundle.Config.LayerDigest,
				DependencyLockDigest:   managedBundle.Config.LockDigest,
				SBOMDigest:             managedSupply.SBOMDigest, SourceBOM: managedBundle.Config.SourceBOM,
				BuilderIdentity: *builderIdentity,
			}
			if _, err := s.PublishManagedAttestation(desc, predicate, managedSigningKey); err != nil {
				return fmt.Errorf("publish final-image managed-dependency attestation: %w", err)
			}
		}
		fmt.Printf("pushed %s (runnable OCI image — kubelet-pullable)\n  index: %s (%d bytes)\n  platforms: %v\n  store: %s\n",
			ref, desc.Digest, desc.Size, artifact.RunnableArches(cfg), *store)
	default:
		return fmt.Errorf("unknown --format %q (want \"image\" or \"artifact\")", *format)
	}
	if cfg.Entry.Mode == "module" {
		fmt.Printf("  entry.mode: module (module=%s)\n", cfg.Entry.Module)
	}
	if len(cfg.Arch) > 0 {
		fmt.Printf("  arch: %v (non-portable; steered onto matching nodes)\n", cfg.Arch)
	}
	if len(cpLayers) > 0 {
		fmt.Printf("  classpath layers: %d (deduped by digest)\n", len(cpLayers))
	}
	if managedBundle != nil {
		fmt.Printf("  managed dependency bundle: %s@%s\n", managedBundle.Config.Name, managedBundle.ManifestDigest)
		fmt.Printf("  source BOM: %s\n", managedBundle.Config.SourceBOM)
		fmt.Printf("  bundle signer: %s\n", managedSupply.BuilderIdentity)
		fmt.Printf("  final-image attestation: signed by %s\n", *builderIdentity)
	}
	if len(mpLayers) > 0 {
		fmt.Printf("  modulepath layers: %d (deduped by digest)\n", len(mpLayers))
	}
	if cfg.CDS != nil && cfg.CDS.Archive != "" {
		fmt.Printf("  cds archive: %s (mounted /app/%s; -Xshare:auto, best-effort)\n", cfg.CDS.Archive, cfg.CDS.Archive)
	}
	fmt.Printf("  -> developer shipped ONLY the JAR; no Dockerfile, no base image.\n")
	return nil
}

// splitArchFlag parses a comma-separated --arch value into a trimmed,
// non-empty token slice.
func splitArchFlag(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// resolveJavaBinary picks the java executable for --appcds training. An explicit
// hint may be either the java binary itself or a JAVA_HOME directory (in which
// case bin/java is appended); it must exist. With no hint, $JAVA_HOME/bin/java is
// used when JAVA_HOME is set, otherwise "java" is resolved on PATH.
func resolveJavaBinary(hint string) (string, error) {
	if hint = strings.TrimSpace(hint); hint != "" {
		if fi, err := os.Stat(hint); err == nil && fi.IsDir() {
			cand := filepath.Join(hint, "bin", "java")
			if _, err := os.Stat(cand); err != nil {
				return "", fmt.Errorf("--appcds-java %q: %w", hint, err)
			}
			return cand, nil
		}
		if _, err := os.Stat(hint); err != nil {
			return "", fmt.Errorf("--appcds-java %q: %w", hint, err)
		}
		return hint, nil
	}
	if jh := strings.TrimSpace(os.Getenv("JAVA_HOME")); jh != "" {
		cand := filepath.Join(jh, "bin", "java")
		if _, err := os.Stat(cand); err == nil {
			return cand, nil
		}
	}
	p, err := exec.LookPath("java")
	if err != nil {
		return "", fmt.Errorf("--appcds: no java found (set --appcds-java or JAVA_HOME, or put java on PATH): %w", err)
	}
	return p, nil
}

func cmdInspect(args []string) error {
	fs := flag.NewFlagSet("inspect", flag.ExitOnError)
	store := fs.String("store", "./oci", "OCI layout directory")
	trustedPublicKey := fs.String("trusted-public-key", "", "optional trusted ECDSA P-256 public key for attestation verification")
	trustedSignerIdentity := fs.String("trusted-signer-identity", "", "expected attestation builder identity")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 {
		return fmt.Errorf("usage: inspect <ref>")
	}
	s := artifact.Store{Root: *store}
	man, _, err := s.ResolveManifestByRef(pos[0])
	if err != nil {
		return err
	}
	mb, _ := json.MarshalIndent(man, "", "  ")
	if man.ArtifactType == artifact.DependencyBundleArtifactType {
		bundle, err := s.ResolveDependencyBundle(pos[0])
		if err != nil {
			return err
		}
		cfg, _ := json.MarshalIndent(bundle.Config, "", "  ")
		lock, _ := json.MarshalIndent(bundle.Lock, "", "  ")
		fmt.Printf("== kind ==\nmanaged dependency bundle\n\n== manifest ==\n%s\n\n== bundle config ==\n%s\n\n== dependency lock ==\n%s\n", mb, cfg, lock)
		if *trustedPublicKey != "" || *trustedSignerIdentity != "" {
			if *trustedPublicKey == "" || *trustedSignerIdentity == "" {
				return fmt.Errorf("bundle verification requires both --trusted-public-key and --trusted-signer-identity")
			}
			key, err := artifact.LoadECDSAPublicKey(*trustedPublicKey)
			if err != nil {
				return err
			}
			verified, err := s.VerifyBundleSupplyChain(bundle, key, *trustedSignerIdentity)
			if err != nil {
				return err
			}
			fmt.Printf("\n== bundle supply chain (signed, verified) ==\n  signer: %s\n  sbom: %s\n",
				verified.BuilderIdentity, verified.SBOMDigest)
		}
		return nil
	}
	var cfg artifact.JVMConfig
	kind := "native artifact"
	if man.IsRunnableImage() {
		kind = "runnable OCI image (kubelet-pullable)"
		cfg, err = man.RunnableConfig()
	} else {
		cb, rerr := s.ReadBlob(man.Config.Digest)
		if rerr != nil {
			return rerr
		}
		cfg, err = artifact.DecodeConfig(cb)
	}
	if err != nil {
		return err
	}
	cb, _ := json.MarshalIndent(cfg, "", "  ")
	fmt.Printf("== kind ==\n%s\n\n== manifest ==\n%s\n\n== jvm config ==\n%s\n", kind, mb, cb)
	if evidence, ok, evidenceErr := man.ManagedDependencyEvidence(); evidenceErr != nil {
		return evidenceErr
	} else if ok {
		raw, _ := json.MarshalIndent(evidence, "", "  ")
		fmt.Printf("\n== managed dependency evidence (unsigned) ==\n%s\n", raw)
	}
	if *trustedPublicKey != "" || *trustedSignerIdentity != "" {
		if *trustedPublicKey == "" || *trustedSignerIdentity == "" {
			return fmt.Errorf("attestation verification requires both --trusted-public-key and --trusted-signer-identity")
		}
		key, err := artifact.LoadECDSAPublicKey(*trustedPublicKey)
		if err != nil {
			return err
		}
		desc, err := s.DescriptorByRef(pos[0])
		if err != nil {
			return err
		}
		predicate, err := s.VerifyManagedAttestation(desc, key, *trustedSignerIdentity)
		if err != nil {
			return err
		}
		raw, _ := json.MarshalIndent(predicate, "", "  ")
		fmt.Printf("\n== managed dependency attestation (signed, verified) ==\n%s\n", raw)
	}
	return nil
}

func cmdDependencyBundle(args []string) error {
	fs := flag.NewFlagSet("dependency-bundle", flag.ExitOnError)
	storeRoot := fs.String("store", "./oci", "OCI layout directory")
	name := fs.String("name", "", "stable bundle name")
	bundleVersion := fs.String("version", "", "bundle version")
	sourceBOM := fs.String("source-bom", "", "source Maven BOM in groupId:artifactId:version syntax")
	lockFile := fs.String("lock", "", "canonical dependency-lock JSON file")
	compatibleJDKs := fs.String("compatible-jdks", "", "optional comma-separated compatible JDK feature versions")
	signingKey := fs.String("signing-key", "", "PEM PKCS#8 ECDSA P-256 key used to sign bundle provenance")
	signerIdentity := fs.String("signer-identity", "", "bundle builder identity recorded in signed provenance")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 2 || *lockFile == "" || *signingKey == "" || strings.TrimSpace(*signerIdentity) == "" {
		return fmt.Errorf("usage: dependency-bundle <classpath-tar> <ref> --name NAME --version VERSION --source-bom G:A:V --lock FILE --signing-key PEM --signer-identity IDENTITY")
	}
	lockRaw, err := os.ReadFile(*lockFile)
	if err != nil {
		return fmt.Errorf("read --lock: %w", err)
	}
	lock, err := artifact.DecodeDependencyLock(lockRaw)
	if err != nil {
		return err
	}
	jdks, err := parseJDKFeatures(*compatibleJDKs)
	if err != nil {
		return err
	}
	store := artifact.Store{Root: *storeRoot}
	desc, err := store.PushDependencyBundle(pos[1], artifact.DependencyBundleConfig{
		Name:           *name,
		Version:        *bundleVersion,
		SourceBOM:      *sourceBOM,
		CompatibleJDKs: jdks,
	}, lock, pos[0])
	if err != nil {
		return err
	}
	key, err := artifact.LoadECDSAPrivateKey(*signingKey)
	if err != nil {
		return err
	}
	bundle, err := store.ResolveDependencyBundle(pos[1])
	if err != nil {
		return err
	}
	sbomDigest, err := store.PublishBundleSupplyChain(desc, bundle.Config, bundle.Lock, key, *signerIdentity)
	if err != nil {
		return fmt.Errorf("publish bundle supply chain: %w", err)
	}
	fmt.Printf("pushed managed dependency bundle %s\n  manifest: %s\n  artifactType: %s\n  store: %s\n",
		pos[1], desc.Digest, artifact.DependencyBundleArtifactType, *storeRoot)
	fmt.Printf("  sbom: %s\n  provenance: signed by %s\n", sbomDigest, *signerIdentity)
	return nil
}

func parseJDKFeatures(value string) ([]int, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	var out []int
	for _, token := range strings.Split(value, ",") {
		token = strings.TrimSpace(token)
		feature, err := strconv.Atoi(token)
		if err != nil || feature <= 0 {
			return nil, fmt.Errorf("--compatible-jdks entry %q must be a positive integer", token)
		}
		out = append(out, feature)
	}
	return out, nil
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	store := fs.String("store", "./oci", "OCI layout directory")
	jdkRoot := fs.String("jdk-root", "", "node JDK home (default: JAVA_HOME)")
	launcher := fs.String("launcher", "java", "launcher name (deployment descriptor's brewlet.sh/launcher; \"java\" for vanilla)")
	appcdsRegen := fs.Bool("appcds-regenerate", false, "opt into node-side AppCDS regeneration (https://github.com/brewlet/site/blob/main/docs/appcds.md §4.3): maintain a per-(artifact,JDK-build) archive cache with -XX:+AutoCreateSharedArchive, self-healing on every JDK patch. This is the deployment/fleet equivalent of spec.jvm.cds.regenerate; any shipped archive becomes optional seed data")
	flagArgs, extra := splitDoubleDash(args)
	pos, err := parseInterspersed(fs, flagArgs)
	if err != nil {
		return err
	}
	if len(pos) < 1 {
		return fmt.Errorf("usage: run <ref>")
	}
	ref := pos[0]

	s := artifact.Store{Root: *store}
	blobs, err := s.ResolveBlobs(ref)
	if err != nil {
		return err
	}
	cfg := blobs.Config

	// Pull: the payload is already in the store (native artifact) or staged from
	// the runnable image's layers; mount it into a sandbox.
	cdsSrc := blobs.CDSHostPath
	sandbox, jarPath, err := runtime.AssembleSandboxWithCDS(cfg, blobs.JarHostPath, blobs.ClasspathHostPaths, blobs.ModulepathHostPaths, cdsSrc, *appcdsRegen)
	if err != nil {
		return err
	}
	defer os.RemoveAll(sandbox)

	plan, err := runtime.BuildPlan(cfg, jarPath, *jdkRoot, *launcher, extra, *appcdsRegen)
	if err != nil {
		return err
	}

	// Node-side AppCDS regeneration (https://github.com/brewlet/site/blob/main/docs/appcds.md §4.3): the local `run` path
	// executes the JVM directly on the host (no sandbox), so the node cache path
	// is a host path (ArchiveArgDir empty). Prepend the resolved regen args.
	// Regeneration is a deployment/fleet choice (--appcds-regenerate), not read
	// from the artifact.
	if *appcdsRegen {
		seed := ""
		if cdsSrc != "" && cfg.CDS != nil && cfg.CDS.Archive != "" {
			seed = cdsSrc
		}
		dec, derr := runtime.DecideCDSRegen(runtime.RegenParams{
			CacheDir:    os.Getenv("BREWLET_CDS_CACHE"),
			JDKRoot:     plan.JDKHome,
			ArtifactKey: ref,
			SeedArchive: seed,
			MetricsDir:  os.Getenv("BREWLET_METRICS_DIR"),
		})
		if derr != nil {
			return derr
		}
		if len(dec.Args) > 0 {
			plan.Args = append(append([]string{}, dec.Args...), plan.Args...)
		}
	}

	fmt.Printf("[brewlet] node JDK : %s\n", plan.JDKHome)
	if !artifact.IsVanillaLauncher(*launcher) {
		fmt.Printf("[brewlet] launcher : %s (owns JVM tuning)\n", artifact.LauncherName(*launcher))
	}
	fmt.Printf("[brewlet] sandbox  : %s\n", sandbox)
	fmt.Printf("[brewlet] launch   : %s\n", plan.CommandLine())
	fmt.Printf("[brewlet] ----- JVM output below -----\n")
	return plan.Run()
}

func cmdBundle(args []string) error {
	fs := flag.NewFlagSet("bundle", flag.ExitOnError)
	store := fs.String("store", "./oci", "OCI layout directory")
	cpu := fs.String("cpu", "", "CPU limit, e.g. 2 or 500m")
	mem := fs.String("memory", "", "memory limit, e.g. 512Mi or 1Gi")
	jdkRoot := fs.String("jdk-root", "/opt/brewlet/jdks/temurin-21", "node JDK runtime root")
	launcher := fs.String("launcher", "java", "launcher name (deployment descriptor's brewlet.sh/launcher; \"java\" for vanilla)")
	launcherRoot := fs.String("launcher-root", "", "node launcher layer root (custom launcher, e.g. jaz)")
	out := fs.String("out", "./bundle", "output bundle directory")
	appcdsRegen := fs.Bool("appcds-regenerate", false, "opt into node-side AppCDS regeneration (https://github.com/brewlet/site/blob/main/docs/appcds.md §4.3): maintain a per-(artifact,JDK-build) archive cache with -XX:+AutoCreateSharedArchive, self-healing on every JDK patch. Deployment/fleet equivalent of spec.jvm.cds.regenerate")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 {
		return fmt.Errorf("usage: bundle <ref>")
	}
	s := artifact.Store{Root: *store}
	blobs, err := s.ResolveBlobs(pos[0])
	if err != nil {
		return err
	}
	cfg := blobs.Config
	cdsSrc := blobs.CDSHostPath
	if err := runtime.GenerateBundleWithRegen(cfg, *jdkRoot, *launcherRoot, *launcher, blobs.JarHostPath, blobs.ClasspathHostPaths, blobs.ModulepathHostPaths, cdsSrc, *out,
		runtime.Resources{CPULimit: *cpu, MemoryLimit: *mem}, nil, runtime.CDSRegenOptions{Regenerate: *appcdsRegen, ArtifactKey: pos[0], CacheDir: os.Getenv("BREWLET_CDS_CACHE")}); err != nil {
		return err
	}
	fmt.Printf("wrote OCI runtime bundle to %s/config.json\n", *out)
	if !artifact.IsVanillaLauncher(*launcher) {
		fmt.Printf("  launcher: %s (owns JVM tuning)\n", artifact.LauncherName(*launcher))
	}
	fmt.Printf("on a Linux node the shim runs:  runc run -b %s brewlet-<id>\n", *out)
	return nil
}

// cmdJDKs lists the JDKs available across the cluster (vendor, major/minor
// version, architecture) by reading the inventory each Brewlet node advertises.
// It shells out to `kubectl get nodes -o json` and renders the result, so it
// needs a working kubectl/kubeconfig but no in-process Kubernetes client.
func cmdJDKs(args []string) error {
	fs := flag.NewFlagSet("jdks", flag.ExitOnError)
	output := fs.String("output", "table", "output format: table (distinct JDKs), wide (per node), or json")
	kubeconfig := fs.String("kubeconfig", "", "path to kubeconfig (default: kubectl's own resolution)")
	context := fs.String("context", "", "kubeconfig context to use")
	selector := fs.String("selector", "", "label selector to filter nodes (kubectl -l)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	kubeArgs := []string{"get", "nodes", "-o", "json"}
	if *kubeconfig != "" {
		kubeArgs = append(kubeArgs, "--kubeconfig", *kubeconfig)
	}
	if *context != "" {
		kubeArgs = append(kubeArgs, "--context", *context)
	}
	if *selector != "" {
		kubeArgs = append(kubeArgs, "-l", *selector)
	}

	cmd := exec.Command("kubectl", kubeArgs...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("kubectl get nodes failed: %s", msg)
		}
		return fmt.Errorf("running kubectl (is it installed and configured?): %w", err)
	}

	nodes, err := inventory.ParseNodes(out)
	if err != nil {
		return err
	}

	switch *output {
	case "table":
		inventory.RenderTable(os.Stdout, nodes)
	case "wide":
		inventory.RenderByNode(os.Stdout, nodes)
	case "json":
		return inventory.RenderJSON(os.Stdout, nodes)
	default:
		return fmt.Errorf("unknown --output %q (want table, wide, or json)", *output)
	}
	return nil
}

func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	namespace := fs.String("namespace", "default", "namespace where the developer will create JavaApplication resources")
	output := fs.String("output", "table", "output format: table or json")
	kubeconfig := fs.String("kubeconfig", "", "path to kubeconfig (default: kubectl's own resolution)")
	context := fs.String("context", "", "kubeconfig context to use")
	if err := fs.Parse(args); err != nil {
		return err
	}

	executor := func(kubectlArgs ...string) ([]byte, error) {
		cmd := exec.Command("kubectl", kubectlArgs...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			if _, lookupErr := exec.LookPath("kubectl"); lookupErr != nil {
				return out, fmt.Errorf("kubectl is not installed or not on PATH")
			}
		}
		return out, err
	}
	report := doctor.Run(executor, doctor.Options{
		Kubeconfig: *kubeconfig,
		Context:    *context,
		Namespace:  *namespace,
	})

	switch *output {
	case "table":
		for _, check := range report.Checks {
			fmt.Printf("[%-4s] %-20s %s\n", strings.ToUpper(string(check.Status)), check.Name, check.Detail)
			if check.Remediation != "" && check.Status != doctor.Pass {
				fmt.Printf("       fix: %s\n", check.Remediation)
			}
		}
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown --output %q (want table or json)", *output)
	}

	if !report.OK() {
		return fmt.Errorf("doctor found one or more blocking checks")
	}
	return nil
}
