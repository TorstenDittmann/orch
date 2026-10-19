#!/usr/bin/env python3
"""Install, exercise and reboot two explicitly supplied disposable VMs.

Requires root SSH, peer/firewall connectivity, a persistent test controller,
a selected target (or --context), and local Pkl. Never use production hosts.
Evidence includes private join artifacts; do not publish the whole directory.
"""
import argparse
import json
from pathlib import Path
import shlex
import subprocess
import time


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--orch", required=True)
    parser.add_argument("--linux-binary", required=True)
    parser.add_argument("--ssh-config", required=True)
    parser.add_argument("--context")
    parser.add_argument("--evidence", required=True, help="new private directory")
    parser.add_argument("--ubuntu", nargs=2, metavar=("SSH_HOST", "PRIVATE_IP"), required=True)
    parser.add_argument("--fedora", nargs=2, metavar=("SSH_HOST", "PRIVATE_IP"), required=True)
    parser.add_argument("--reboot-disposable-vms", action="store_true", required=True)
    args = parser.parse_args()
    evidence = Path(args.evidence).resolve()
    evidence.mkdir(mode=0o700)
    executable = str(Path(args.orch).resolve())
    common = ["--context", args.context] if args.context else []

    def run(command, check=True):
        result = subprocess.run(command, text=True, capture_output=True, timeout=900)
        if check and result.returncode:
            raise RuntimeError(f"command failed ({result.returncode}): {shlex.join(command)}\n{result.stderr}")
        return result

    def remote(host, *command, check=True):
        return run(["ssh", "-F", args.ssh_config, "-o", "ConnectTimeout=10", "-o", "BatchMode=yes", host, shlex.join(command)], check)

    def cli(*command):
        return run([executable, *command, *common])

    def record(name, result):
        (evidence / (name + ".stdout")).write_text(result.stdout)
        (evidence / (name + ".stderr")).write_text(result.stderr)
        return result

    nodes = [("ubuntu", *args.ubuntu), ("fedora", *args.fedora)]
    installed = {}
    for distro, host, address in nodes:
        remote(host, "sh", "-ec", "test $(id -u) = 0; test ! -e /var/lib/orch-install; test ! -e /opt/orch")
        if distro == "fedora":
            assert remote(host, "getenforce").stdout.strip() == "Enforcing"
            assert remote(host, "systemctl", "is-active", "firewalld").stdout.strip() == "active"
        node = "install-test-" + distro
        join = evidence / (distro + ".join")
        record(distro + "-join", cli("node", "join", "create", node, "--ttl", "2h", "--output", str(join), "--json"))
        run(["scp", "-F", args.ssh_config, args.linux_binary, host + ":/root/orch-install-test"])
        run(["scp", "-F", args.ssh_config, str(join), host + ":/root/orch-install-test.join"])
        command = ["/root/orch-install-test", "node", "install", "--advertise-address", address, "--json"]
        record(distro + "-doctor-before", remote(host, "/root/orch-install-test", "node", "doctor", "--advertise-address", address, "--json", check=False))
        record(distro + "-dry-run", remote(host, *command, "--dry-run"))
        remote(host, "test", "!", "-e", "/var/lib/orch-install")
        installed[distro] = json.loads(record(distro + "-install", remote(host, *command, "--join-file", "/root/orch-install-test.join")).stdout)
        assert installed[distro]["ready"]
        before = remote(host, "systemctl", "show", "orch-agent", "--property=MainPID", "--value").stdout
        replay = json.loads(record(distro + "-rerun", remote(host, *command)).stdout)
        assert replay["ready"] and not replay["changed"]
        assert before == remote(host, "systemctl", "show", "orch-agent", "--property=MainPID", "--value").stdout
        cli("node", "label", node, "install-os=" + distro)
        cli("node", "ingress", node, "--enable")

    manifest = evidence / "smoke.pkl"
    services = []
    for distro, _, _ in nodes:
        services.append('''  ["%s"] {
    image = "docker.io/library/busybox@sha256:9db7b59979c38555a39def84a31fb98b5296952f9e3afd4f6f11f05b07adfab0"
    command { "/bin/sh" }
    args { "-c"; "mkdir -p /www; echo ok >/www/healthz; exec httpd -f -p 8080 -h /www" }
    resources { cpu = "100m"; memory = "64Mi" }
    constraints { ["install-os"] = "%s" }
    http = new Http { port = 8080; healthPath = "/healthz" }
  }''' % (distro, distro))
    manifest.write_text('amends "https://orch.torsten.work/pkl/v1alpha1/Application.pkl"\nname = "install-smoke"\nservices {\n' + "\n".join(services) + "\n}\n")
    record("deploy", cli("deploy", "--root", str(evidence), "--file", str(manifest), "--wait", "--timeout", "10m", "--json"))
    jobs = {}
    for distro, host, _ in nodes:
        command = [executable, "job", "run", "install-smoke", distro, "--wait", "--json", *common, "--", "/bin/sh", "-c", "echo installed-job"]
        jobs[distro] = json.loads(record(distro + "-job", run(command)).stdout)
        assert jobs[distro]["status"]["state"] == 3
    for _, host, _ in nodes:
        remote(host, "systemctl", "reboot", check=False)

    time.sleep(15)
    for distro, host, address in nodes:
        deadline = time.monotonic() + 600
        while time.monotonic() < deadline:
            result = remote(host, "/opt/orch/bin/orch", "node", "install", "--advertise-address", address, "--wait-timeout", "30s", "--json", check=False)
            if result.returncode == 0:
                record(distro + "-after-reboot", result)
                assert json.loads(result.stdout)["node_id"] == installed[distro]["node_id"]
                break
            time.sleep(5)
        else:
            raise RuntimeError(distro + " did not recover after reboot")
        after = json.loads(record(distro + "-job-after-reboot", cli("job", "show", jobs[distro]["id"], "--json")).stdout)
        for field in ["state", "started_at", "finished_at"]:
            assert after["status"][field] == jobs[distro]["status"][field]
        record(distro + "-doctor-after", remote(host, "/opt/orch/bin/orch", "node", "doctor", "--advertise-address", address, "--json"))
        if distro == "fedora":
            assert remote(host, "getenforce").stdout.strip() == "Enforcing"
            assert remote(host, "systemctl", "is-active", "firewalld").stdout.strip() == "active"
            record("fedora-avc", remote(host, "ausearch", "-m", "AVC,USER_AVC", "-ts", "boot", check=False))
    record("deployment-after-reboot", cli("app", "watch", "install-smoke", "--timeout", "10m", "--json"))
    record("nodes-after-reboot", cli("nodes", "--json"))
    print("Install, no-op rerun, service, job and reboot checks passed. Private evidence:", evidence)


if __name__ == "__main__":
    main()
