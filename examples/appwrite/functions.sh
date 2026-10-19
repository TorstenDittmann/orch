#!/bin/sh
# Runs Appwrite's function executor and build orchestrator on this
# container's own Docker daemon, the way Appwrite's docker-compose.yml runs
# them on the host's Docker. orch delivers this file to the privileged
# `functions` service as the functions-script secret.
set -eu

# Only the local socket: a TCP listener would hand the daemon to every
# workload on the private network.
dockerd-entrypoint.sh dockerd --host=unix:///var/run/docker.sock &
daemon=$!
until docker info >/dev/null 2>&1; do
	kill -0 "$daemon"
	sleep 1
done

# The executor reads deployment code from the same bucket as Appwrite.
encode() {
	printf '%s' "$1" | sed -e 's/%/%25/g' -e 's|/|%2F|g' -e 's/+/%2B/g' -e 's/:/%3A/g' \
		-e 's/@/%40/g' -e 's/=/%3D/g' -e 's/&/%26/g' -e 's/?/%3F/g'
}
credentials="$(encode "$_APP_STORAGE_S3_ACCESS_KEY"):$(encode "$_APP_STORAGE_S3_SECRET")"
region="$_APP_STORAGE_S3_REGION"
if [ -n "$_APP_STORAGE_S3_ENDPOINT" ]; then
	storage="s3://$credentials@localhost?region=$region&url=$(encode "$_APP_STORAGE_S3_ENDPOINT")"
else
	storage="s3://$credentials@$_APP_STORAGE_S3_BUCKET.s3.$region.amazonaws.com?region=$region"
fi

for network in runtimes appwrite; do
	docker network inspect "$network" >/dev/null 2>&1 || docker network create "$network" >/dev/null
done
docker rm -f exc1 orchestrator >/dev/null 2>&1 || true

# Appwrite authenticates to the orchestrator with the jobs secret. The
# orchestrator reads its key only from a file. Docker turns a missing bind
# source into a directory when it restarts the old container at boot.
rm -rf /run/orchestrator-api-key
(umask 077 && printf '%s' "$_APP_JOBS_SECRET" >/run/orchestrator-api-key)

# Port 80 serves the executor and 8080 the orchestrator, both at this
# service's address. The executor joins the runtimes network itself.
docker run -d --name exc1 --hostname exc1 --network appwrite --restart unless-stopped \
	--stop-signal SIGINT -p 80:80 \
	-v /var/run/docker.sock:/var/run/docker.sock -v /tmp:/tmp \
	-e OPR_EXECUTOR_ENV=production \
	-e OPR_EXECUTOR_SECRET="$_APP_EXECUTOR_SECRET" \
	-e OPR_EXECUTOR_NETWORK=runtimes \
	-e OPR_EXECUTOR_RUNTIMES="$_APP_FUNCTIONS_RUNTIMES,$_APP_SITES_RUNTIMES" \
	-e OPR_EXECUTOR_RUNTIME_VERSIONS=v5 \
	-e OPR_EXECUTOR_INACTIVE_THRESHOLD=60 \
	-e OPR_EXECUTOR_MAINTENANCE_INTERVAL=3600 \
	-e OPR_EXECUTOR_CONNECTION_STORAGE="$storage" \
	-e OPEN_RUNTIMES_NFT=enabled \
	"$EXECUTOR_IMAGE" >/dev/null
docker run -d --name orchestrator --hostname orchestrator --network appwrite --restart unless-stopped \
	--user root -p 8080:8080 \
	-v /var/run/docker.sock:/var/run/docker.sock \
	-v /run/orchestrator-api-key:/run/secrets/api-key:ro \
	-e API_KEY_FILE=/run/secrets/api-key \
	-e ORCHESTRATOR_BACKEND=docker \
	-e ORCHESTRATOR_NETWORK=appwrite \
	-e DOCKER_NETWORK=appwrite \
	-e ARTIFACT_ENDPOINT=http://orchestrator:8080 \
	-e S3_ENDPOINT="$_APP_STORAGE_S3_ENDPOINT" \
	-e S3_REGION="$region" \
	-e S3_ACCESS_KEY_ID="$_APP_STORAGE_S3_ACCESS_KEY" \
	-e S3_SECRET_ACCESS_KEY="$_APP_STORAGE_S3_SECRET" \
	"$ORCHESTRATOR_IMAGE" >/dev/null

docker logs -f exc1 &
docker logs -f orchestrator &
wait "$daemon"
