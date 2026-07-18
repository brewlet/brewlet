# Launchers

A **launcher** is the program that fronts your entrypoint — the thing that actually
becomes `java …`. Brewlet is launcher-agnostic: you can use the stock OpenJDK `java`
launcher, or a drop-in, java-compatible launcher such as **`jaz`** (the
[Azure Command Launcher for Java](https://learn.microsoft.com/java/jaz/overview))
that auto-tunes the JVM for you.

Crucially, **Brewlet injects no `-XX` tuning flags in either case**. The difference
between launchers is *who does the tuning*.

Related: [Resource tuning](resource-tuning.md) · [JDK management](jdk-management.md).

---

## The two launchers

### Vanilla `java` (default) — you tune it

The container-aware JDK reads the cgroup limits; you set heap/GC/etc. explicitly via
the descriptor's `jvm.args`. The artifact only carries app-intrinsic launch knobs.

```yaml
jvm:
  version: 21
  launcher: java                 # default; may be omitted
  args:                          # tuning is YOUR responsibility
    - "-XX:MaxRAMPercentage=75.0"
    - "-XX:+UseZGC"
    - "-XX:+ExitOnOutOfMemoryError"
```

### `jaz` — it tunes for you

`jaz` inspects the container's resources and picks sensible JVM ergonomics
automatically, so you typically pass **no** manual tuning flags. Don't restate what
`jaz` derives (e.g. `MaxRAMPercentage`); reserve `args` for genuinely app-specific
flags only.

```yaml
jvm:
  version: 21
  launcher: jaz                  # auto-tunes heap/GC/CPU from the cgroup limits
  # no MaxRAMPercentage / GC selection needed — jaz derives them
  # args: ["-Dfoo=bar"]          # only truly app-specific flags, if any
```

| | Vanilla `java` | `jaz` |
|---|---|---|
| Who tunes heap/GC/CPU | you (via descriptor `jvm.args`) | `jaz`, from the cgroup limits |
| Node install needed | none — every JDK ships `java` | a launcher layer on the node ([below](#installing-jaz-on-nodes)) |
| Composes over any JDK | n/a | yes — finds the JVM via `JAVA_HOME` |
| Best for | full manual control | hands-off, sensible defaults |

> If a pod requests a launcher a node doesn't have, it fails admission with
> `NoCompatibleLauncher` ([Troubleshooting](troubleshooting.md)).

---

## How launcher selection is resolved

At launch, Brewlet resolves the launcher binary like this
([`internal/runtime/launch.go`](https://github.com/brewlet/brewlet/blob/main/internal/runtime/launch.go)):

- **Vanilla** (`launcher` omitted or `"java"`): use the selected JDK's own
  `<jdk-home>/bin/java`.
- **Custom** (e.g. `"jaz"`): resolve it as an absolute path, or find it on `PATH`
  from the node-installed launcher layer. A missing launcher surfaces as
  `NoCompatibleLauncher`.

Brewlet always pins `JAVA_HOME` to the selected node JDK, so a launcher like `jaz`
finds the right JVM. Optional `launcher.args` are placed **ahead** of everything, and
`launcher.env` is exported. This is why **one launcher layer composes over any
installed OpenJDK distribution**.

Arg ordering for a custom launcher:

```
<launcher> <launcher.args…>
  <artifact launch knobs: --enable-preview/--add-*/-D…>
  <descriptor jvm.args…> <extra args…> -jar /app/app.jar
```

(For the vanilla `java` launcher, `launcher.args` are not applied — `java` gets
artifact launch knobs, descriptor `jvm.args`, extras, and the entrypoint.)

---

## Installing `jaz` on nodes

Launchers are installed the same declarative way as JDKs, but **independently** of
them. Declare the inventory:

```yaml
# Helm values
provisioner:
  launchers: "jaz"     # empty = vanilla java only
```

The provisioner stages each launcher under:

```
/opt/brewlet/launchers/<name>/bin/<name>
```

`jaz` is **not** part of any JDK — it's a separate Linux package. Preferred install
is **copy-from-image** (the Microsoft Build of OpenJDK images ship `jaz`
preinstalled), so the host package manager is untouched:

```bash
ctr image pull mcr.microsoft.com/openjdk/jdk:25-ubuntu
mkdir -p /opt/brewlet/launchers/jaz/bin
ctr run --rm --mount type=bind,src=/opt/brewlet/launchers/jaz,dst=/out,options=rbind:rw \
  mcr.microsoft.com/openjdk/jdk:25-ubuntu cp -a /usr/bin/jaz /out/bin/jaz
```

Or install the package for the node OS and stage the binary:

```bash
# Azure Linux
sudo tdnf install -y jaz
# Ubuntu/Debian (after adding the Microsoft repo)
sudo apt-get install -y jaz
# then stage it into the launcher root:
mkdir -p /opt/brewlet/launchers/jaz/bin && cp -a "$(command -v jaz)" /opt/brewlet/launchers/jaz/bin/
```

If a copied launcher needs shared libraries not present in the JDK root, include them
under the launcher root (e.g. `lib/`); the layer is mounted read-only alongside the
JDK.

The node advertises installed launchers:

```bash
kubectl get node node-1 -o jsonpath='{.metadata.annotations.brewlet\.sh/launchers}{"\n"}'
# java,jaz
```

See the core runtime's
[`provisioner/README.md`](https://github.com/brewlet/brewlet/blob/main/provisioner/README.md)
for the provisioner mechanics.

---

## Requesting a launcher for a workload

Per-pod, request a launcher via annotation (validated + scheduled by the webhook):

```yaml
metadata:
  annotations:
    brewlet.sh/launcher: "jaz"     # omit or "java" for the vanilla launcher
spec:
  runtimeClassName: brewlet
  containers:
    - image: registry.example.com/demo/hello:1.0.0
```

Or, in a `JavaApplication` descriptor:

```yaml
spec:
  jvm:
    version: 21
    launcher: jaz
```

The deployment descriptor is authoritative; launchers are not recorded in
`jvm-config.json`.

---

## Choosing between them

- Use **`jaz`** when you want hands-off, resource-aware defaults and don't want to
  hand-maintain `-XX` flags across services.
- Use **vanilla `java`** when you need precise, explicit control over GC and heap, or
  can't install a launcher layer on your nodes.

Either way, see [Resource tuning](resource-tuning.md) for what the limits actually do
to the JVM.
