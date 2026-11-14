#!/usr/bin/env python3
"""Execute one round plan of the KVM variance experiment on the benchmark host.

Run as root on codedang8. Every run places its execution flows on socket 0:

  b0   bare metal, one flow pinned per logical CPU (m flows, k = 1)
  b1   bare metal, m groups of k CPUs, k flows float inside each group
  vk   m KVM guests with k vCPUs and k flows each
  vk1  m KVM guests with k + 1 vCPUs and k flows each

Flows run closed loop behind one barrier and stay busy until every flow has at
least MIN_STEADY samples inside the all-flows-active window. Results are
written below <out>/<run-id>/ and are resumable: completed runs are skipped.
"""
import argparse
import ctypes
import glob
import json
import os
import shutil
import signal
import socket
import subprocess
import sys
import threading
import time
import traceback

STAGE = os.environ.get("KVMEXP_STAGE", "/var/tmp/iris-kvm")
QEMU = "/usr/bin/qemu-system-x86_64"
POLICIES = {
    # Production-like: SMT on, turbo on. Fill physical cores, then siblings.
    "prod": {"smt": 1, "no_turbo": 0, "housekeeping": [0, 16],
             "slots": list(range(1, 8)) + list(range(17, 24))},
    # Quiet: SMT off, turbo off.
    "quiet": {"smt": 0, "no_turbo": 1, "housekeeping": [0],
              "slots": list(range(1, 8))},
}
MIN_STEADY = 52          # >= 50 complete samples survive window trimming
WARMUP = 3
COOLDOWN_S = 10
IDLE_BUSY_MAX = 0.02
MAX_MEASURE_S = 1500
CAT_USABLE_WAYS = 18     # ways 0-17; ways 18-19 are the shareable c0000 bits
CAT_DOMAIN = 0           # socket 0 L3 domain
PR_SET_THP_DISABLE = 41

libc = ctypes.CDLL("libc.so.6", use_errno=True)


def now_ns():
    return time.time_ns()


def read(path, default=None):
    try:
        with open(path) as f:
            return f.read().strip()
    except OSError:
        return default


def write(path, text):
    with open(path, "w") as f:
        f.write(text)


def dump(path, obj):
    tmp = path + ".tmp"
    with open(tmp, "w") as f:
        json.dump(obj, f, indent=1, sort_keys=True)
    os.replace(tmp, path)


def log(msg):
    print(time.strftime("%H:%M:%S"), msg, flush=True)


# ---------------------------------------------------------------- host state

def host_state():
    cpus = range(0, 32)
    return {
        "smt_active": read("/sys/devices/system/cpu/smt/active"),
        "smt_control": read("/sys/devices/system/cpu/smt/control"),
        "no_turbo": read("/sys/devices/system/cpu/intel_pstate/no_turbo"),
        "governors": sorted({g for c in cpus
                             if (g := read(f"/sys/devices/system/cpu/cpu{c}/cpufreq/scaling_governor"))}),
        "online": read("/sys/devices/system/cpu/online"),
        "node0_cpus": read("/sys/devices/system/node/node0/cpulist"),
        "thp": read("/sys/kernel/mm/transparent_hugepage/enabled"),
        "numa_balancing": read("/proc/sys/kernel/numa_balancing"),
        "kernel": os.uname().release,
        "resctrl_schemata": read("/sys/fs/resctrl/schemata"),
        "resctrl_groups": sorted(d for d in os.listdir("/sys/fs/resctrl")
                                 if os.path.isdir(f"/sys/fs/resctrl/{d}")
                                 and d not in ("info", "mon_data", "mon_groups")),
        "loadavg": read("/proc/loadavg"),
    }


def verify_policy(name):
    p = POLICIES[name]
    st = host_state()
    errs = []
    if st["smt_active"] != str(p["smt"]):
        errs.append(f"smt active {st['smt_active']} != {p['smt']}")
    if st["no_turbo"] != str(p["no_turbo"]):
        errs.append(f"no_turbo {st['no_turbo']} != {p['no_turbo']}")
    if st["governors"] != ["performance"]:
        errs.append(f"governors {st['governors']}")
    if st["resctrl_schemata"] is None:
        errs.append("resctrl not mounted")
    if st["resctrl_groups"]:
        errs.append(f"stray resctrl groups {st['resctrl_groups']}")
    if errs:
        raise SystemExit("host policy mismatch: " + "; ".join(errs))
    return st


def socket0_cpus():
    out = []
    for part in read("/sys/devices/system/node/node0/cpulist").split(","):
        a, _, b = part.partition("-")
        out += list(range(int(a), int(b or a) + 1))
    return out


def cpu_times(cpus):
    res = {}
    with open("/proc/stat") as f:
        for line in f:
            if line.startswith("cpu") and line[3].isdigit():
                parts = line.split()
                c = int(parts[0][3:])
                if c in cpus:
                    v = list(map(int, parts[1:9]))
                    res[c] = (sum(v), v[3] + v[4])
    return res


def busy_fraction(cpus, dt):
    a = cpu_times(cpus)
    time.sleep(dt)
    b = cpu_times(cpus)
    tot = sum(b[c][0] - a[c][0] for c in cpus)
    idle = sum(b[c][1] - a[c][1] for c in cpus)
    return 1 - idle / tot if tot else 0.0


def stray_processes():
    out = subprocess.run(["pgrep", "-f", "qemu-system-x86_64|judger-bench|libjudger"],
                         capture_output=True, text=True).stdout.split()
    return [p for p in out if int(p) != os.getpid()]


def idle_gate():
    time.sleep(COOLDOWN_S)
    cpus = socket0_cpus()
    for attempt in range(1, 61):
        stray = stray_processes()
        busy = busy_fraction(cpus, 5)
        if not stray and busy < IDLE_BUSY_MAX:
            return {"attempts": attempt, "socket0_busy": round(busy, 4),
                    "loadavg": read("/proc/loadavg")}
        log(f"idle gate attempt {attempt}: busy={busy:.3f} stray={stray}")
    raise RuntimeError("host did not become idle")


# ---------------------------------------------------------------- layout / CAT

def layout(arm, k, m, slots):
    if arm == "b0":
        groups, flows = [[s] for s in slots[:m]], 1
        width = 1
    else:
        width = k + 1 if arm == "vk1" else k
        groups, flows = [slots[j * width:(j + 1) * width] for j in range(m)], k
    if any(len(g) != width for g in groups) or width * len(groups) > len(slots):
        raise ValueError(f"layout {arm} k={k} m={m} exceeds {len(slots)} slots")
    return groups, flows


def cat_setup(run_id, ngroups):
    ways = CAT_USABLE_WAYS // ngroups
    if ways < 1:
        raise ValueError("too many CAT groups")
    full = read("/sys/fs/resctrl/info/L3/cbm_mask")
    names = []
    for j in range(ngroups):
        mask = ((1 << ways) - 1) << (j * ways)
        name = f"kvmexp-g{j:02d}"
        os.mkdir(f"/sys/fs/resctrl/{name}")
        names.append(name)
        domains = [d.split("=")[0] for d in read("/sys/fs/resctrl/schemata").split(":", 1)[1].split(";")]
        line = ";".join(f"{d}={mask:x}" if int(d) == CAT_DOMAIN else f"{d}={full}" for d in domains)
        write(f"/sys/fs/resctrl/{name}/schemata", f"L3:{line}\n")
        got = read(f"/sys/fs/resctrl/{name}/schemata")
        if f"{CAT_DOMAIN}={mask:x}" not in got.replace(" ", ""):
            raise RuntimeError(f"schemata not applied for {name}: {got}")
    return names, ways


def cat_cleanup():
    for d in glob.glob("/sys/fs/resctrl/kvmexp-g*"):
        try:
            os.rmdir(d)
        except OSError as e:
            log(f"resctrl cleanup {d}: {e}")


# ---------------------------------------------------------------- sampler

class Sampler(threading.Thread):
    def __init__(self, path, cpus, groups):
        super().__init__(daemon=True)
        self.path, self.cpus, self.groups = path, cpus, groups
        self.stop_evt = threading.Event()

    def mon(self, base):
        d = f"{base}/mon_data/mon_L3_{CAT_DOMAIN:02d}"
        return {k: int(read(f"{d}/{k}", "0") or 0)
                for k in ("llc_occupancy", "mbm_total_bytes", "mbm_local_bytes")}

    def run(self):
        with open(self.path, "w") as f:
            while not self.stop_evt.is_set():
                rec = {"t": now_ns(),
                       "freq_khz": {c: int(read(f"/sys/devices/system/cpu/cpu{c}/cpufreq/scaling_cur_freq", "0"))
                                    for c in self.cpus},
                       "root": self.mon("/sys/fs/resctrl")}
                for g in self.groups:
                    rec[g] = self.mon(f"/sys/fs/resctrl/{g}")
                f.write(json.dumps(rec) + "\n")
                f.flush()
                self.stop_evt.wait(1.0)


# ---------------------------------------------------------------- flow streams

class Streams:
    """Incrementally parse flow NDJSON streams and convert to host time."""

    def __init__(self):
        self.files = {}   # name -> [path, offset_ns, pos, samples]

    def add(self, name, path, offset_ns=0):
        self.files[name] = [path, offset_ns, 0, []]

    def set_offset(self, name, offset_ns):
        self.files[name][1] = offset_ns

    def poll(self):
        for ent in self.files.values():
            path, off, pos, samples = ent
            try:
                with open(path, "rb") as f:
                    f.seek(pos)
                    data = f.read()
            except FileNotFoundError:
                continue
            end = data.rfind(b"\n")
            if end < 0:
                continue
            for line in data[:end].splitlines():
                if line.strip():
                    s = json.loads(line)
                    samples.append((s["startedAtNs"] + off, s["endedAtNs"] + off, s["status"]))
            ent[2] = pos + end + 1

    def steady_counts(self):
        firsts = [e[3][0][0] for e in self.files.values() if e[3]]
        if len(firsts) < len(self.files):
            return None, 0
        ws = max(firsts)
        return ws, min(sum(1 for s in e[3] if s[0] >= ws) for e in self.files.values())

    def failures(self):
        return sum(1 for e in self.files.values() for s in e[3] if s[2] != "success")


# ---------------------------------------------------------------- VM control

class QMP:
    def __init__(self, path, timeout=120):
        deadline = time.time() + timeout
        while True:
            try:
                self.sock = socket.socket(socket.AF_UNIX)
                self.sock.settimeout(timeout)
                self.sock.connect(path)
                break
            except OSError:
                self.sock.close()
                if time.time() > deadline:
                    raise
                time.sleep(0.1)
        self.f = self.sock.makefile("rw")
        json.loads(self.f.readline())
        self.cmd("qmp_capabilities")

    def cmd(self, name, **args):
        msg = {"execute": name}
        if args:
            msg["arguments"] = args
        self.f.write(json.dumps(msg) + "\n")
        self.f.flush()
        while True:
            r = json.loads(self.f.readline())
            if "return" in r:
                return r["return"]
            if "error" in r:
                raise RuntimeError(f"QMP {name}: {r['error']}")

    def close(self):
        self.sock.close()


class Guest:
    def __init__(self, rundir, run_id, idx, vcpus, flows, workload, cpus, housekeeping, streams):
        self.idx, self.vcpus, self.flows, self.cpus = idx, vcpus, flows, cpus
        self.dir = f"{rundir}/vm{idx:02d}"
        os.makedirs(self.dir)
        self.mem = 1024 + 256 * flows
        self.lines, self.cv = [], threading.Condition()
        append = " ".join([
            "root=/dev/vda", "ro", "console=ttyS0", "quiet", "loglevel=3",
            "init=/opt/kvmexp/init", f"kvmexp.flows={flows}", f"kvmexp.workload={workload}",
            f"kvmexp.run={run_id}", f"kvmexp.vm={idx:02d}", f"kvmexp.warmup={WARMUP}"])
        args = [QEMU, "-name", f"vm{idx:02d},debug-threads=on",
                "-machine", "q35,accel=kvm,memory-backend=ram0",
                "-cpu", "host,migratable=off",
                "-smp", f"{vcpus},sockets=1,cores={vcpus},threads=1",
                "-m", f"{self.mem}M",
                "-object", f"memory-backend-ram,id=ram0,size={self.mem}M,host-nodes=0,policy=bind,prealloc=on,merge=off",
                "-nodefaults", "-no-user-config", "-display", "none", "-S", "-no-reboot",
                "-qmp", f"unix:{self.dir}/qmp.sock,server=on,wait=off",
                "-serial", f"file:{self.dir}/console.log",
                "-kernel", f"{STAGE}/images/vmlinuz", "-append", append,
                "-drive", f"file={STAGE}/images/rootfs.img,format=raw,if=none,id=root,readonly=on",
                "-device", "virtio-blk-pci,drive=root",
                "-device", "virtio-serial-pci",
                "-chardev", f"socket,id=ctl,path={self.dir}/ctl.sock,server=on,wait=off",
                "-device", "virtserialport,chardev=ctl,name=ctl"]
        self.flow_names = []
        for f in range(1, flows + 1):
            name = f"vm{idx:02d}-flow-{f:02d}"
            path = f"{rundir}/{name}.ndjson"
            args += ["-chardev", f"file,id=f{f:02d},path={path}",
                     "-device", f"virtserialport,chardev=f{f:02d},name=flow-{f:02d}"]
            streams.add(name, path)
            self.flow_names.append(name)
        self.args = args
        hk = set(housekeeping)

        def pre():
            libc.prctl(PR_SET_THP_DISABLE, 1, 0, 0, 0)
            os.sched_setaffinity(0, hk)
            os.setsid()

        self.proc = subprocess.Popen(args, stdin=subprocess.DEVNULL,
                                     stdout=open(f"{self.dir}/qemu.log", "w"),
                                     stderr=subprocess.STDOUT, preexec_fn=pre)

    def pin_and_start(self, cat_group):
        self.qmp = QMP(f"{self.dir}/qmp.sock")
        vcpus = self.qmp.cmd("query-cpus-fast")
        self.tids = {}
        for v in vcpus:
            tid = v["thread-id"]
            cpu = self.cpus[v["cpu-index"]]
            os.sched_setaffinity(tid, {cpu})
            if cat_group:
                write(f"/sys/fs/resctrl/{cat_group}/tasks", f"{tid}\n")
            self.tids[v["cpu-index"]] = tid
        if len(self.tids) != self.vcpus:
            raise RuntimeError("vCPU count mismatch")
        self.pinning = {str(i): {"tid": t, "cpu": self.cpus[i],
                                 "affinity": sorted(os.sched_getaffinity(t))}
                        for i, t in self.tids.items()}
        self.ctl = socket.socket(socket.AF_UNIX)
        self.ctl.connect(f"{self.dir}/ctl.sock")
        threading.Thread(target=self._reader, daemon=True).start()
        self.qmp.cmd("cont")

    def _reader(self):
        buf = b""
        while True:
            try:
                data = self.ctl.recv(65536)
            except OSError:
                data = b""
            if not data:
                with self.cv:
                    self.lines.append("EOF")
                    self.cv.notify_all()
                return
            buf += data
            while b"\n" in buf:
                line, buf = buf.split(b"\n", 1)
                with self.cv:
                    self.lines.append(line.decode(errors="replace"))
                    self.cv.notify_all()

    def expect(self, word, timeout):
        deadline = time.time() + timeout
        with self.cv:
            while True:
                for i, line in enumerate(self.lines):
                    if line.split(" ", 1)[0] in (word, "FAIL", "EOF"):
                        del self.lines[i]
                        if not line.startswith(word):
                            raise RuntimeError(f"vm{self.idx:02d}: {line}")
                        return line
                left = deadline - time.time()
                if left <= 0:
                    raise TimeoutError(f"vm{self.idx:02d}: waiting for {word}")
                self.cv.wait(left)

    def send(self, line):
        self.ctl.sendall((line + "\n").encode())

    def schedstat(self):
        out = {}
        for i, tid in self.tids.items():
            run, wait, slices = read(f"/proc/{self.proc.pid}/task/{tid}/schedstat", "0 0 0").split()
            out[str(i)] = {"run_ns": int(run), "wait_ns": int(wait), "timeslices": int(slices)}
        return out

    def anon_huge_kb(self):
        for line in (read(f"/proc/{self.proc.pid}/smaps_rollup", "") or "").splitlines():
            if line.startswith("AnonHugePages:"):
                return int(line.split()[1])
        return None

    def numa_pages(self):
        txt = read(f"/proc/{self.proc.pid}/numa_maps", "") or ""
        tot = {}
        for line in txt.splitlines():
            for tok in line.split():
                if tok.startswith("N") and "=" in tok:
                    n, v = tok[1:].split("=")
                    tot[n] = tot.get(n, 0) + int(v)
        return tot

    def stop_and_wait(self, timeout=180):
        try:
            self.proc.wait(timeout=timeout)
        except subprocess.TimeoutExpired:
            self.kill()

    def kill(self):
        if self.proc.poll() is None:
            os.killpg(self.proc.pid, signal.SIGKILL)
            self.proc.wait()


def parse_stat(line):
    parts = line.split()
    # "cpu user nice system idle iowait irq softirq steal guest guest_nice"
    names = ["user", "nice", "system", "idle", "iowait", "irq", "softirq", "steal"]
    return dict(zip(names, map(int, parts[1:9])))


# ---------------------------------------------------------------- one run

def run_vm(spec, rundir, groups, flows, pol, streams, cat_groups, meta):
    vcpus = len(groups[0])
    guests = []
    try:
        for j, cpus in enumerate(groups, 1):
            guests.append(Guest(rundir, spec["run_id"], j, vcpus, flows, spec["workload"], cpus,
                                pol["housekeeping"], streams))
        for j, g in enumerate(guests):
            g.pin_and_start(cat_groups[j] if cat_groups else None)
        t_boot = time.time()
        for g in guests:
            g.expect("READY", 300)
        meta["boot_to_ready_s"] = round(time.time() - t_boot, 2)
        sched0 = {g.idx: g.schedstat() for g in guests}
        for g in guests:
            g.send(f"START {now_ns()}")
        meta["guests"] = {}
        for g in guests:
            parts = g.expect("STARTED", 60).split()
            host_ns, guest_ns = int(parts[1]), int(parts[2])
            for name in g.flow_names:
                streams.set_offset(name, host_ns - guest_ns)
            meta["guests"][g.idx] = {"cpus": g.cpus, "vcpus": g.vcpus, "mem_mib": g.mem,
                                     "pinning": g.pinning, "offset_ns": host_ns - guest_ns,
                                     "stat_start": parse_stat(" ".join(parts[3:]))}
        meta["start_ns"] = now_ns()
        t_start = time.time()
        memory_checked = False
        while True:
            for g in guests:
                if g.proc.poll() is not None:
                    raise RuntimeError(f"vm{g.idx:02d} exited early")
            if not memory_checked and time.time() - t_start > 5:
                # Guest RAM must be 4 KiB-backed (THP disabled) and node-0 local.
                for g in guests:
                    meta["guests"][g.idx]["anon_huge_kb"] = g.anon_huge_kb()
                    meta["guests"][g.idx]["numa_pages"] = g.numa_pages()
                memory_checked = True
            reason = measure_tick(streams, t_start)
            if reason:
                break
            time.sleep(0.5)
        meta["stop_reason"] = reason
        sched1 = {g.idx: g.schedstat() for g in guests}
        for g in guests:
            g.send(f"STOP {now_ns()}")
        meta["stop_ns"] = now_ns()
        failed_flows = 0
        for g in guests:
            parts = g.expect("STOPPED", 300).split()
            info = meta["guests"][g.idx]
            info["stop_offset_ns"] = int(parts[1]) - int(parts[2])
            info["stat_stop"] = parse_stat(" ".join(parts[3:]))
            failed_flows += int(g.expect("DONE", 60).split()[1])
            info["schedstat_start"], info["schedstat_stop"] = sched0[g.idx], sched1[g.idx]
        meta["failed_flows"] = failed_flows
        for g in guests:
            g.stop_and_wait()
            meta["guests"][g.idx]["qemu_rc"] = g.proc.returncode
        meta["qemu_args_vm01"] = guests[0].args
    finally:
        for g in guests:
            g.kill()


def run_bare(spec, rundir, groups, flows, pol, streams, cat_groups, meta):
    procs, n = [], 0
    env = dict(os.environ, KVMEXP_STAGE=STAGE)
    try:
        for j, cpus in enumerate(groups):
            for _ in range(flows):
                n += 1
                nn = f"{n:02d}"
                streams.add(f"flow-{nn}", f"{rundir}/flow-{nn}.ndjson")
                cmd = ["bash", f"{STAGE}/scripts/host-flow.sh", rundir, spec["run_id"], nn,
                       spec["workload"], ",".join(map(str, cpus)), str(WARMUP)]
                if cat_groups:
                    cmd.append(cat_groups[j])
                procs.append(subprocess.Popen(cmd, env=env, stdin=subprocess.DEVNULL,
                                              stdout=open(f"{rundir}/host-flow-{nn}.log", "w"),
                                              stderr=subprocess.STDOUT, start_new_session=True))
        deadline = time.time() + 300
        for i in range(1, n + 1):
            while not os.path.exists(f"{rundir}/ready-{i:02d}"):
                if procs[i - 1].poll() is not None:
                    raise RuntimeError(f"flow {i:02d} failed during warm-up")
                if time.time() > deadline:
                    raise TimeoutError("bare flows not ready")
                time.sleep(0.05)
        meta["start_ns"] = now_ns()
        write(f"{rundir}/start", "")
        t_start = time.time()
        while True:
            if any(p.poll() is not None for p in procs):
                raise RuntimeError("bare flow exited early")
            reason = measure_tick(streams, t_start)
            if reason:
                break
            time.sleep(0.5)
        meta["stop_reason"] = reason
        meta["stop_ns"] = now_ns()
        write(f"{rundir}/stop", "")
        failed = 0
        for p in procs:
            try:
                failed += p.wait(timeout=300) != 0
            except subprocess.TimeoutExpired:
                failed += 1
        meta["failed_flows"] = failed
    finally:
        for p in procs:
            if p.poll() is None:
                os.killpg(p.pid, signal.SIGKILL)
                p.wait()
        cleanup_sandboxes(spec["run_id"])
        shutil.rmtree(f"/dev/shm/kvmexp-{spec['run_id']}", ignore_errors=True)


def cleanup_sandboxes(run_id):
    for path in sorted(glob.glob(f"/sys/fs/cgroup/sandbox-{run_id}-*")):
        for child in glob.glob(f"{path}/box-*"):
            try:
                os.rmdir(child)
            except OSError as e:
                log(f"cgroup cleanup {child}: {e}")
        try:
            os.rmdir(path)
        except OSError as e:
            log(f"cgroup cleanup {path}: {e}")


def measure_tick(streams, t_start):
    streams.poll()
    _, count = streams.steady_counts()
    if count >= MIN_STEADY:
        return "steady"
    if time.time() - t_start > MAX_MEASURE_S:
        return "timeout"
    return None


def execute(spec, out):
    run_id = spec["run_id"]
    rundir = f"{out}/{run_id}"
    pol = POLICIES[spec["policy"]]
    groups, flows = layout(spec["arm"], spec["k"], spec["m"], pol["slots"])
    os.makedirs(rundir)
    meta = {"spec": spec, "groups": groups, "flows_per_group": flows,
            "total_flows": flows * len(groups), "housekeeping": pol["housekeeping"],
            "min_steady": MIN_STEADY, "warmup": WARMUP, "started": time.strftime("%FT%T%z")}
    meta["host"] = verify_policy(spec["policy"])
    meta["idle_gate"] = idle_gate()
    cat_groups = []
    sampler = None
    try:
        if spec.get("cat"):
            cat_groups, ways = cat_setup(run_id, len(groups))
            meta["cat"] = {"groups": cat_groups, "ways_per_group": ways,
                           "schemata": {g: read(f"/sys/fs/resctrl/{g}/schemata") for g in cat_groups}}
        worker_cpus = sorted(c for g in groups for c in g)
        sampler = Sampler(f"{rundir}/sampler.ndjson", worker_cpus, cat_groups)
        sampler.start()
        streams = Streams()
        fn = run_vm if spec["arm"] in ("vk", "vk1") else run_bare
        fn(spec, rundir, groups, flows, pol, streams, cat_groups, meta)
        streams.poll()
        ws, count = streams.steady_counts()
        meta["window_start_ns"], meta["min_steady_count"] = ws, count
        meta["failed_samples"] = streams.failures()
        meta["status"] = "ok" if (meta["stop_reason"] == "steady" and meta["failed_flows"] == 0
                                  and meta["failed_samples"] == 0) else "degraded"
    finally:
        if sampler:
            sampler.stop_evt.set()
            sampler.join(5)
        cat_cleanup()
        meta["finished"] = time.strftime("%FT%T%z")
        dump(f"{rundir}/meta.json", meta)
    return meta["status"]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--plan", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--retries", type=int, default=1)
    args = ap.parse_args()
    if os.geteuid() != 0:
        raise SystemExit("must run as root")
    plan = json.load(open(args.plan))
    os.makedirs(args.out, exist_ok=True)
    progress = f"{args.out}/progress.json"
    state = {"total": len(plan["runs"]), "done": 0, "ok": 0, "degraded": 0, "failed": [],
             "plan": os.path.abspath(args.plan), "started": time.strftime("%FT%T%z")}
    signal.signal(signal.SIGTERM, lambda *_: sys.exit(143))
    cat_cleanup()
    for spec in plan["runs"]:
        rid = spec["run_id"]
        meta_path = f"{args.out}/{rid}/meta.json"
        if os.path.exists(meta_path) and json.load(open(meta_path)).get("status") in ("ok", "degraded"):
            state["done"] += 1
            continue
        status = "failed"
        for attempt in range(args.retries + 1):
            rundir = f"{args.out}/{rid}"
            if os.path.exists(rundir):
                os.rename(rundir, f"{rundir}.fail-{int(time.time())}")
            state["current"] = rid
            dump(progress, state)
            log(f"run {rid} attempt {attempt + 1}")
            try:
                status = execute(spec, args.out)
            except SystemExit:
                raise
            except Exception as e:  # recorded and retried; the round continues
                log(f"run {rid} failed: {e!r}")
                with open(f"{args.out}/errors.log", "a") as f:
                    f.write(f"{time.strftime('%FT%T')} {rid} attempt {attempt + 1}: {e!r}\n")
                    traceback.print_exc(file=f)
                status = "failed"
            if status != "failed":
                break
        state["done"] += 1
        if status == "failed":
            state["failed"].append(rid)
        else:
            state[status] += 1
        log(f"run {rid} -> {status} ({state['done']}/{state['total']})")
        dump(progress, state)
    state["current"] = None
    state["finished"] = time.strftime("%FT%T%z")
    dump(progress, state)


if __name__ == "__main__":
    main()
