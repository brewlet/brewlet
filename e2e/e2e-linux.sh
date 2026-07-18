set -e
echo "== installing runc on the node =="
apt-get update -qq >/dev/null 2>&1 && apt-get install -y -qq runc >/dev/null 2>&1
runc --version | head -1

echo "== provisioner installs the JDK runtime root (simulated) =="
mkdir -p /opt/brewlet/jdks
ln -sfn /opt/java/openjdk /opt/brewlet/jdks/temurin-21
echo "  /opt/brewlet/jdks/temurin-21 -> $(readlink -f /opt/brewlet/jdks/temurin-21)"

echo "== kubelet/CRI hands the shim the image config + pod limits =="
cat > /tmp/ic.json <<JSON
{ "storeRoot":"/work/oci","ref":"demo/hello:1.0.0","jdkRootsDir":"/opt/brewlet/jdks","cpuLimit":"1","memoryLimit":"384Mi" }
JSON

echo "== shim Create(): disassemble artifact -> OCI runtime bundle =="
rm -rf /tmp/bundle-e2e
/work/bin/shim-linux prepare-bundle /tmp/ic.json /tmp/bundle-e2e

echo "== assemble sandbox rootfs (the JDK runtime root provides the userland) =="
mkdir -p /tmp/bundle-e2e/rootfs
cp -a /bin /sbin /lib /usr /etc /tmp/bundle-e2e/rootfs/ 2>/dev/null || true
[ -e /lib64 ] && cp -a /lib64 /tmp/bundle-e2e/rootfs/ 2>/dev/null || true
mkdir -p /tmp/bundle-e2e/rootfs/opt/jdk /tmp/bundle-e2e/rootfs/app /tmp/bundle-e2e/rootfs/proc /tmp/bundle-e2e/rootfs/tmp
touch /tmp/bundle-e2e/rootfs/app/app.jar

echo "== shim Start(): runc runs java -jar under cgroups =="
# cgroup v2 nesting: move existing procs out of the root cgroup and delegate
# controllers, so runc can create a child cgroup for the sandbox.
mkdir -p /sys/fs/cgroup/init
for p in $(cat /sys/fs/cgroup/cgroup.procs 2>/dev/null); do echo "$p" > /sys/fs/cgroup/init/cgroup.procs 2>/dev/null || true; done
echo "+cpu +memory +pids" > /sys/fs/cgroup/cgroup.subtree_control 2>/dev/null || true
cd /tmp/bundle-e2e
runc run brewlet-e2e &
for i in $(seq 1 30); do curl -sf localhost:8080/healthz >/dev/null 2>&1 && break; sleep 0.5; done
echo "--- /info (cgroup limits seen by the JVM under runc) ---"
curl -s localhost:8080/info
echo "--- /hello ---"
curl -s localhost:8080/hello
runc kill brewlet-e2e KILL 2>/dev/null || true
sleep 1
runc delete --force brewlet-e2e 2>/dev/null || true
echo "== done =="

echo
echo "== modular (JPMS) scenario: shim -> runc -> java -p ... -m ... =="
echo "== kubelet/CRI hands the shim the modular image config =="
cat > /tmp/ic-mod.json <<JSON
{ "storeRoot":"/work/oci","ref":"demo/orders:1.0.0","jdkRootsDir":"/opt/brewlet/jdks","cpuLimit":"1","memoryLimit":"384Mi" }
JSON

echo "== shim Create(): disassemble the modular artifact -> OCI runtime bundle =="
rm -rf /tmp/bundle-mod
/work/bin/shim-linux prepare-bundle /tmp/ic-mod.json /tmp/bundle-mod

echo "== assemble sandbox rootfs (mount targets: /app/orders.jar file + /app/mods dir) =="
mkdir -p /tmp/bundle-mod/rootfs
cp -a /bin /sbin /lib /usr /etc /tmp/bundle-mod/rootfs/ 2>/dev/null || true
[ -e /lib64 ] && cp -a /lib64 /tmp/bundle-mod/rootfs/ 2>/dev/null || true
mkdir -p /tmp/bundle-mod/rootfs/opt/jdk /tmp/bundle-mod/rootfs/app/mods /tmp/bundle-mod/rootfs/proc /tmp/bundle-mod/rootfs/tmp
touch /tmp/bundle-mod/rootfs/app/orders.jar

echo "== shim Start(): runc runs java -p <module-path> -m <module> under cgroups =="
cd /tmp/bundle-mod
runc run brewlet-e2e-mod &
for i in $(seq 1 30); do curl -sf localhost:8080/healthz >/dev/null 2>&1 && break; sleep 0.5; done
echo "--- modular /hello (produced by the com.example.greeter module on the module path) ---"
curl -s localhost:8080/hello
echo "--- modular /info (both modules resolved) ---"
curl -s localhost:8080/info
runc kill brewlet-e2e-mod KILL 2>/dev/null || true
sleep 1
runc delete --force brewlet-e2e-mod 2>/dev/null || true
echo "== modular done =="
