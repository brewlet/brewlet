set -e
# Real Linux mechanism for the MIXED class-path + module-path launch form
# (docs/layered-classpath-deployment.md §8.1, docs/jpms-support.md §6.3): the shim
# disassembles a modular Brewlet artifact that ALSO carries a supplementary
# class-path layer. The main modular JAR is bind-mounted at /app/orders.jar, the
# module layer is unpacked at /app/mods, and the class-path layer is unpacked at
# /app/lib. runc then runs, as PID 1 under real cgroup limits:
#
#   java -cp /app/lib/* -p /app/orders.jar:/app/mods -m com.example.orders/...
#
# So this tier exercises BOTH `-cp` and `-p` on one launch — the module path
# resolves the com.example.greeter module while the class path carries the plain,
# non-modular com.example.legacy.Legacy helper. OrdersApp's /info reports the live
# java.class.path and confirms the legacy class is reachable, proving the mixed
# assembly end-to-end.
#
# Runs inside the privileged eclipse-temurin container launched by tier 3.

echo "== installing runc on the node =="
apt-get update -qq >/dev/null 2>&1 && apt-get install -y -qq runc curl >/dev/null 2>&1
runc --version | head -1

echo "== provisioner installs the JDK runtime root (simulated) =="
mkdir -p /opt/brewlet/jdks
ln -sfn /opt/java/openjdk /opt/brewlet/jdks/temurin-21
echo "  /opt/brewlet/jdks/temurin-21 -> $(readlink -f /opt/brewlet/jdks/temurin-21)"

echo "== kubelet/CRI hands the shim the mixed-form image config + pod limits =="
cat > /tmp/ic-mixed.json <<JSON
{ "storeRoot":"/work/oci","ref":"demo/orders-mixed:1.0.0","jdkRootsDir":"/opt/brewlet/jdks","cpuLimit":"1","memoryLimit":"384Mi" }
JSON

echo "== shim Create(): disassemble the mixed artifact -> OCI runtime bundle =="
# prepare-bundle mounts the main modular JAR at /app/orders.jar, UNPACKS the
# module layer into /app/mods AND the class-path layer into /app/lib.
rm -rf /tmp/bundle-mixed
/work/bin/shim-linux prepare-bundle /tmp/ic-mixed.json /tmp/bundle-mixed

echo "== assemble sandbox rootfs (mount targets: /app/orders.jar + /app/mods + /app/lib) =="
mkdir -p /tmp/bundle-mixed/rootfs
cp -a /bin /sbin /lib /usr /etc /tmp/bundle-mixed/rootfs/ 2>/dev/null || true
[ -e /lib64 ] && cp -a /lib64 /tmp/bundle-mixed/rootfs/ 2>/dev/null || true
mkdir -p /tmp/bundle-mixed/rootfs/opt/jdk /tmp/bundle-mixed/rootfs/app/mods \
    /tmp/bundle-mixed/rootfs/app/lib /tmp/bundle-mixed/rootfs/proc /tmp/bundle-mixed/rootfs/tmp
touch /tmp/bundle-mixed/rootfs/app/orders.jar

echo "== shim Start(): runc runs java -cp /app/lib/* -p ...:/app/mods -m ... under cgroups =="
# cgroup v2 nesting: move existing procs out of the root cgroup and delegate
# controllers, so runc can create a child cgroup for the sandbox.
mkdir -p /sys/fs/cgroup/init
for p in $(cat /sys/fs/cgroup/cgroup.procs 2>/dev/null); do echo "$p" > /sys/fs/cgroup/init/cgroup.procs 2>/dev/null || true; done
echo "+cpu +memory +pids" > /sys/fs/cgroup/cgroup.subtree_control 2>/dev/null || true
cd /tmp/bundle-mixed
echo "--- launch args (from the OCI runtime bundle: expect both -cp and -p) ---"
grep -o '"args":\[[^]]*\]' config.json | head -1
runc run brewlet-e2e-mixed &
for i in $(seq 1 30); do curl -sf localhost:8080/healthz >/dev/null 2>&1 && break; sleep 0.5; done
echo "--- mixed /hello (com.example.greeter module resolved on the module path) ---"
curl -s localhost:8080/hello
echo "--- mixed /info (java.class.path populated + legacy class reachable on -cp) ---"
curl -s localhost:8080/info
runc kill brewlet-e2e-mixed KILL 2>/dev/null || true
sleep 1
runc delete --force brewlet-e2e-mixed 2>/dev/null || true
echo "== mixed done =="
