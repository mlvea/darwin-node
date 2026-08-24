# Standalone repo

darwin-node is the repository root. There is no parent macOS-vz-kubelet
project in this tree.

Do not search, edit, or assume `macOS-vz-kubelet/darwin-node`. A sibling
checkout of Agoda's macOS-vz-kubelet (if present) is a different git
repository used only for lineage comparison.

`go.mod` lives here. GitHub Actions workflows live in `.github/workflows/`
here. `make` targets run here with no extra `cd`.
