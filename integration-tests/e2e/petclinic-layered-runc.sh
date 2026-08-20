set -e
# Real Linux mechanism for the *layered classpath* Spring PetClinic: the shim
# disassembles the Brewlet artifact into an OCI runtime bundle — the thin
# application JAR is bind-mounted at /app/spring-petclinic-app.jar and the
# dependency layer(s) are unpacked and bind-mounted read-only at /app/lib — then
# runc runs `java -cp /app/spring-petclinic-app.jar:/app/lib/* <MainClass>` as
# PID 1 under real cgroup limits, using a node-resident JDK.
#
# This is the layered twin of e2e/petclinic-runc.sh (fat-JAR / `java -jar`): same
# node mechanism, but the business code and the dependencies travel as separate
# OCI layers so only the small app-JAR layer redeploys when your code changes.
#
# Runs inside the privileged eclipse-temurin container launched by tier 7.

echo "== installing runc on the node =="
apt-get update -qq >/dev/null 2>&1 && apt-get install -y -qq runc curl >/dev/null 2>&1
runc --version | head -1

echo "== provisioner installs the JDK runtime root (simulated) =="
# PetClinic targets Java 17; the descriptor/CRI metadata requests temurin-17.
mkdir -p /opt/brewlet/jdks
ln -sfn /opt/java/openjdk /opt/brewlet/jdks/temurin-17
echo "  /opt/brewlet/jdks/temurin-17 -> $(readlink -f /opt/brewlet/jdks/temurin-17)"

echo "== kubelet/CRI hands the shim the image config + pod limits =="
cat > /tmp/ic-pcl.json <<JSON
{ "storeRoot":"/work/oci","ref":"demo/petclinic-layered:1.0.0","jdkRootsDir":"/opt/brewlet/jdks","jdkRequest":"temurin-17","cpuLimit":"1","memoryLimit":"768Mi" }
JSON

echo "== shim Create(): disassemble layered artifact -> OCI runtime bundle =="
# prepare-bundle reads the artifact from the store: it mounts the thin app JAR at
# /app/spring-petclinic-app.jar and UNPACKS the classpath dependency layer(s)
# into a host dir bind-mounted read-only at /app/lib.
rm -rf /tmp/bundle-pcl
/work/bin/shim-linux prepare-bundle /tmp/ic-pcl.json /tmp/bundle-pcl

echo "== assemble sandbox rootfs (mount targets: /app/spring-petclinic-app.jar + /app/lib) =="
mkdir -p /tmp/bundle-pcl/rootfs
cp -a /bin /sbin /lib /usr /etc /tmp/bundle-pcl/rootfs/ 2>/dev/null || true
[ -e /lib64 ] && cp -a /lib64 /tmp/bundle-pcl/rootfs/ 2>/dev/null || true
mkdir -p /tmp/bundle-pcl/rootfs/opt/jdk /tmp/bundle-pcl/rootfs/app/lib /tmp/bundle-pcl/rootfs/proc /tmp/bundle-pcl/rootfs/tmp
# The shim bind-mounts the thin app JAR here and /app/lib (the unpacked deps).
touch /tmp/bundle-pcl/rootfs/app/spring-petclinic-app.jar

echo "== shim Start(): runc runs java -cp app.jar:lib/* <MainClass> under cgroups =="
# cgroup v2 nesting: move existing procs out of the root cgroup and delegate
# controllers, so runc can create a child cgroup for the sandbox.
mkdir -p /sys/fs/cgroup/init
for p in $(cat /sys/fs/cgroup/cgroup.procs 2>/dev/null); do echo "$p" > /sys/fs/cgroup/init/cgroup.procs 2>/dev/null || true; done
echo "+cpu +memory +pids" > /sys/fs/cgroup/cgroup.subtree_control 2>/dev/null || true
cd /tmp/bundle-pcl
echo "--- launch args (from the OCI runtime bundle) ---"
grep -o '"args":\[[^]]*\]' config.json | head -1
runc run brewlet-petclinic-layered &
# Spring Boot on a single cgroup CPU takes longer to boot than the demo app.
for i in $(seq 1 90); do curl -sf localhost:8080/actuator/health >/dev/null 2>&1 && break; sleep 1; done
echo "--- /actuator/health (Spring Boot up under runc, layered classpath) ---"
curl -s localhost:8080/actuator/health
echo
echo "--- welcome page <title> (served by the real PetClinic app) ---"
curl -s localhost:8080/ | grep -i -o '<title>[^<]*</title>' | head -1
echo "--- system.cpu.count (JVM is cgroup-aware: reads the sandbox limits) ---"
curl -s localhost:8080/actuator/metrics/system.cpu.count 2>/dev/null | head -c 200
echo
runc kill brewlet-petclinic-layered KILL 2>/dev/null || true
sleep 1
runc delete --force brewlet-petclinic-layered 2>/dev/null || true
echo "== petclinic-layered done =="
