#!/usr/bin/env python3
"""Exercise controller installation on two explicitly disposable VMs.

Each VM is also enrolled as a worker to prove path independence. The hosts must
have root SSH, internet access, and the supplied address assigned locally. This
script stops services and reboots both hosts; never point it at persistent hosts.
"""
import argparse
import json
import os
from pathlib import Path
import shlex
import subprocess
import time


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--orch", required=True, help="operator-machine orch executable")
    parser.add_argument("--linux-binary", required=True)
    parser.add_argument("--linux-pkl", required=True)
    parser.add_argument("--ssh-config", required=True)
    parser.add_argument("--evidence", required=True, help="new private directory")
    parser.add_argument("--ubuntu", nargs=3, metavar=("SSH_HOST", "ADDRESS", "PORT"), required=True)
    parser.add_argument("--fedora", nargs=3, metavar=("SSH_HOST", "ADDRESS", "PORT"), required=True)
    parser.add_argument("--reboot-disposable-vms", action="store_true", required=True)
    args = parser.parse_args()
    evidence = Path(args.evidence).resolve()
    evidence.mkdir(mode=0o700)

    def run(command, check=True, env=None):
        result = subprocess.run(command, text=True, capture_output=True, timeout=1200, env=env)
        if check and result.returncode:
            raise RuntimeError(f"command failed ({result.returncode}): {shlex.join(command)}\n{result.stderr}")
        return result

    def remote(host, script, check=True):
        return run(["ssh", "-F", args.ssh_config, "-o", "BatchMode=yes", host, "sh", "-ec", script], check)

    def record(name, result):
        (evidence / f"{name}.stdout").write_text(result.stdout)
        (evidence / f"{name}.stderr").write_text(result.stderr)
        return result

    for distro, (host, address, port) in (("ubuntu", args.ubuntu), ("fedora", args.fedora)):
        server = f"https://{address}:{port}"
        install = f"/root/orch-test cluster install --name {distro}-test --server {server} --listen 0.0.0.0:{port} --admin-dir /root/orch-admin --recovery-dir /root/orch-recovery"
        remote(host, "test $(id -u) = 0; test ! -e /var/lib/orch-controller-install; test ! -e /opt/orch-controller; test ! -e /opt/orch")
        if distro == "fedora":
            remote(host, "test $(getenforce) = Enforcing; test $(systemctl is-active firewalld) = active; firewall-cmd --list-all > /root/firewall-before")
        run(["scp", "-F", args.ssh_config, args.linux_binary, f"{host}:/root/orch-test"])
        run(["scp", "-F", args.ssh_config, args.linux_pkl, f"{host}:/usr/local/bin/pkl"])
        remote(host, "chown root:root /root/orch-test /usr/local/bin/pkl; chmod 0755 /root/orch-test /usr/local/bin/pkl")
        record(f"{distro}-doctor-before", remote(host, f"/root/orch-test cluster doctor --server {server} --listen 0.0.0.0:{port} --json", False))
        record(f"{distro}-dry-run", remote(host, install + " --dry-run --json"))
        remote(host, "test ! -e /var/lib/orch-controller-install")
        installed = json.loads(record(f"{distro}-install", remote(host, install + " --wait-timeout 5m --json")).stdout)
        assert installed["ready"]
        admin = evidence / f"{distro}-admin"
        run(["scp", "-r", "-F", args.ssh_config, f"{host}:/root/orch-admin", str(admin)])
        operator_env = os.environ.copy()
        operator_env["XDG_CONFIG_HOME"] = str(evidence / f"{distro}-operator-config")
        record(f"{distro}-target-import", run([args.orch, "target", "import", f"{distro}-test", str(admin / "context.json"), "--use", "--json"], env=operator_env))
        record(f"{distro}-target-test", run([args.orch, "target", "test", "--json"], env=operator_env))
        pid = remote(host, "systemctl show orch-controller --property=MainPID --value").stdout.strip()
        rerun = json.loads(record(f"{distro}-rerun", remote(host, install + " --json")).stdout)
        assert rerun["ready"] and not rerun["changed"]
        assert remote(host, "systemctl show orch-controller --property=MainPID --value").stdout.strip() == pid
        remote(host, f"/root/orch-test node join create {distro}-worker --ttl 2h --output /root/worker.join --context /root/orch-admin/context.json; /root/orch-test node install --advertise-address {address} --join-file /root/worker.join --wait-timeout 10m")
        manifest = '''amends "orch:/v1alpha1/Application.pkl"
name = "controller-install-smoke"
services { ["web"] { image = "docker.io/library/busybox@sha256:9db7b59979c38555a39def84a31fb98b5296952f9e3afd4f6f11f05b07adfab0"; command { "/bin/sh" }; args { "-c"; "mkdir -p /www; echo ok >/www/healthz; exec httpd -f -p 8080 -h /www" }; resources { cpu = "100m"; memory = "64Mi" }; http = new Http { port = 8080; healthPath = "/healthz" } } }
'''
        remote(host, "mkdir -p /root/smoke; cat > /root/smoke/orch.pkl <<'EOF'\n" + manifest + "EOF")
        record(f"{distro}-deploy", remote(host, "/root/orch-test deploy --root /root/smoke --file /root/smoke/orch.pkl --wait --timeout 10m --context /root/orch-admin/context.json --json"))
        job = json.loads(record(f"{distro}-job", remote(host, "/root/orch-test job run controller-install-smoke web --wait --context /root/orch-admin/context.json --json -- /bin/sh -c 'echo install-job'")).stdout)
        assert job["status"]["state"] == 3
        remote(host, "test $(curl -fsS http://10.88.0.2:8080/healthz) = ok; systemctl stop orch-controller; sleep 15; test $(curl -fsS http://10.88.0.2:8080/healthz) = ok; systemctl start orch-controller")
        record(f"{distro}-restore", remote(host, "/opt/orch-controller/bin/orch cluster restore --backup /root/orch-recovery/control.db --pki-dir /root/orch-recovery/pki --secret-key /root/orch-recovery/secret-master.key --data-dir /root/orch-restored --json"))
        if distro == "fedora":
            remote(host, "test $(getenforce) = Enforcing; test $(systemctl is-active firewalld) = active; firewall-cmd --list-all > /root/firewall-after; cmp /root/firewall-before /root/firewall-after; ausearch -m AVC,USER_AVC -ts recent > /root/avc || test $? = 1")
        remote(host, "systemctl reboot", False)
        deadline = time.monotonic() + 600
        while time.monotonic() < deadline:
            check = remote(host, f"systemctl is-active --quiet orch-controller; systemctl is-active --quiet orch-agent; /opt/orch-controller/bin/orch cluster doctor --server {server} --listen 0.0.0.0:{port} --json; test $(curl -fsS http://10.88.0.2:8080/healthz) = ok", False)
            if check.returncode == 0:
                record(f"{distro}-after-reboot", check)
                break
            time.sleep(5)
        else:
            raise RuntimeError(f"{distro} did not recover after reboot")
    print("Controller, co-located worker, workload, recovery and reboot checks passed. Private evidence:", evidence)


if __name__ == "__main__":
    main()
