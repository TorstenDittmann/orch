#!/bin/sh
# Creates the SSH key that signs orch releases. Run it once from the
# repository root, then commit packaging/release/allowed_signers.
#
# It stores the private key as the ORCH_RELEASE_SIGNING_KEY repository secret
# with the GitHub CLI when that is installed and signed in, and leaves a copy
# in ./orch-release-signing-key for you to keep somewhere safe (a password
# manager) and then delete. To rotate, run it again: the new key is added to
# allowed_signers, so releases signed with the old key still verify until you
# remove its line.
set -eu

[ -f packaging/release/allowed_signers ] || { echo "run this from the orch repository root" >&2; exit 1; }
[ ! -e orch-release-signing-key ] || { echo "orch-release-signing-key already exists" >&2; exit 1; }
command -v ssh-keygen >/dev/null 2>&1 || { echo "ssh-keygen is required" >&2; exit 1; }

umask 077
ssh-keygen -q -t ed25519 -N '' -C "orch-release $(date -u +%Y-%m-%d)" -f orch-release-signing-key
printf 'orch-release namespaces="orch-release" %s\n' "$(cat orch-release-signing-key.pub)" >> packaging/release/allowed_signers
rm -f orch-release-signing-key.pub

if command -v gh >/dev/null 2>&1 && gh auth status >/dev/null 2>&1; then
	gh secret set ORCH_RELEASE_SIGNING_KEY < orch-release-signing-key
	echo "Stored the private key as the ORCH_RELEASE_SIGNING_KEY repository secret."
else
	echo "Add the contents of ./orch-release-signing-key as the repository secret"
	echo "ORCH_RELEASE_SIGNING_KEY (Settings > Secrets and variables > Actions)."
fi
echo "Commit packaging/release/allowed_signers, keep ./orch-release-signing-key"
echo "somewhere safe, then delete it from this folder."
