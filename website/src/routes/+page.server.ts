import { highlight } from "../lib/server/docs";

const manifest = `amends "https://orch.torsten.work/pkl/v1alpha1/Application.pkl"

name = "api"

services {
  ["web"] {
    image = "ghcr.io/acme/api:1.4"
    replicas = 2
    http = new Http {
      port = 8080
      domains { "api.example.com" }
      healthPath = "/healthz"
    }
  }
}`;

const setup = `# on the controller
sudo orch init

# on your machine
orch join-token worker-1
orch target add prod ssh://root@controller`;

export function load() {
	return {
		manifest: highlight(manifest, "pkl"),
		setup: highlight(setup, "shellscript"),
	};
}
