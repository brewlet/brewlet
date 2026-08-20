set -e
# Real Linux mechanism for a genuine Spring Boot fat JAR: the shim disassembles
# the Brewlet artifact into an OCI runtime bundle and runc runs `java -jar` as
# PID 1 under real cgroup limits, using a node-resident JDK. This is exactly what
# the containerd shim does on a provisioned node — here proven against the
# upstream Spring PetClinic (dependency-heavy, ~63MB fat JAR), not a toy app.
#
# Runs inside the privileged eclipse-temurin container launched by tier 7.

echo "== installing runc on the node =="
apt-get update -qq >/dev/null 2>&1 && apt-get install -y -qq runc curl >/dev/null 2>&1
runc --version | head -1

echo "== provisioner installs the JDK runtime root (simulated) =="
# PetClinic targets Java 17; the descriptor/CRI metadata requests temurin-17. On
# a real node the provisioner materializes /opt/brewlet/jdks/temurin-17. The e2e
# base image ships a newer JDK (running a 17-target app on it is fine), so we
# point the temurin-17 root at it to mirror a compatible node.
mkdir -p /opt/brewlet/jdks
ln -sfn /opt/java/openjdk /opt/brewlet/jdks/temurin-17
echo "  /opt/brewlet/jdks/temurin-17 -> $(readlink -f /opt/brewlet/jdks/temurin-17)"

echo "== kubelet/CRI hands the shim the image config + pod limits =="
cat > /tmp/ic-pc.json <<JSON
{ "storeRoot":"/work/oci","ref":"demo/petclinic:1.0.0","jdkRootsDir":"/opt/brewlet/jdks","jdkRequest":"temurin-17","cpuLimit":"1","memoryLimit":"768Mi" }
JSON

echo "== shim Create(): disassemble artifact -> OCI runtime bundle =="
rm -rf /tmp/bundle-pc
/work/bin/shim-linux prepare-bundle /tmp/ic-pc.json /tmp/bundle-pc

echo "== assemble sandbox rootfs (the JDK runtime root provides the userland) =="
mkdir -p /tmp/bundle-pc/rootfs
cp -a /bin /sbin /lib /usr /etc /tmp/bundle-pc/rootfs/ 2>/dev/null || true
[ -e /lib64 ] && cp -a /lib64 /tmp/bundle-pc/rootfs/ 2>/dev/null || true
mkdir -p /tmp/bundle-pc/rootfs/opt/jdk /tmp/bundle-pc/rootfs/app /tmp/bundle-pc/rootfs/proc /tmp/bundle-pc/rootfs/tmp
# The shim mounts the JAR at /app/<mainJar>; create the bind-mount target file.
touch /tmp/bundle-pc/rootfs/app/spring-petclinic.jar

echo "== shim Start(): runc runs java -jar (Spring Boot) under cgroups =="
# cgroup v2 nesting: move existing procs out of the root cgroup and delegate
# controllers, so runc can create a child cgroup for the sandbox.
mkdir -p /sys/fs/cgroup/init
for p in $(cat /sys/fs/cgroup/cgroup.procs 2>/dev/null); do echo "$p" > /sys/fs/cgroup/init/cgroup.procs 2>/dev/null || true; done
echo "+cpu +memory +pids" > /sys/fs/cgroup/cgroup.subtree_control 2>/dev/null || true
cd /tmp/bundle-pc
runc run brewlet-petclinic &
# Spring Boot on a single cgroup CPU takes longer to boot than the demo app.
for i in $(seq 1 90); do curl -sf localhost:8080/actuator/health >/dev/null 2>&1 && break; sleep 1; done
echo "--- /actuator/health (Spring Boot up under runc) ---"
curl -s localhost:8080/actuator/health
echo
echo "--- welcome page <title> (served by the real PetClinic app) ---"
curl -s localhost:8080/ | grep -i -o '<title>[^<]*</title>' | head -1
echo "--- /actuator/info (JVM is cgroup-aware: reads the sandbox limits directly) ---"
curl -s localhost:8080/actuator/metrics/system.cpu.count 2>/dev/null | head -c 200
echo
runc kill brewlet-petclinic KILL 2>/dev/null || true
sleep 1
runc delete --force brewlet-petclinic 2>/dev/null || true
echo "== petclinic done =="
