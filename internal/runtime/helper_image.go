package runtime

// UtilityImage is the small shell image every file-level helper runs in:
// preparing a seed volume, installing a branch entrypoint, measuring disk use,
// copying and removing hostPath volume directories, the zfs userland.
//
// It is pinned by digest, not by tag. Some of these helpers run as root with
// the storage node's whole data root mounted, so a re-pointed tag (or a
// poisoned pull-through cache) would run someone else's code against every
// branch's data. The digest is the one the Dockerfiles' runtime stage builds
// on, and TestUtilityImageMatchesDockerfiles keeps the two from drifting: a
// base-image bump that forgets this constant fails the unit suite.
//
// The kube driver runs helpers that ask for UtilityImage on
// --kube-helper-image instead when it is set (a mirror in a private or
// air-gapped registry).
const UtilityImage = "alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6"
