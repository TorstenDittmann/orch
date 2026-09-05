import { defineConfig } from "docia";

export default defineConfig({
	srcDir: "../docs",
	outDir: "dist",
	publicDir: "public",
	basePath: "/",
	prettyUrls: true,
	site: {
		title: "orch",
		description: "Application orchestration for small Linux clusters.",
		language: "en",
		url: "https://orch-small-cluster.torsten-dittmann.chatgpt.site",
		ogImage: "/og.png",
		githubEditBaseUrl: "https://change.codes/torsten/better-swarm/files/main/docs",
		githubEditBranch: "main",
		githubEditPath: "docs",
	},
	theme: {
		favicon: "",
		accentColor: "#0f766e",
		customCss: ["/orch.css"],
		colorMode: "system",
	},
});
