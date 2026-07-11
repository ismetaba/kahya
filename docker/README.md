# Docker runtime setup (W3-04)

Kâhya's shell sandbox needs a local Docker daemon. On Apple Silicon we
prefer **colima** (scriptable, no GUI license needed).

## colima (preferred)

```sh
brew install colima docker docker-buildx
colima start --cpu 4 --memory 8 --vm-type vz
docker info   # should succeed once colima is up
```

`make docker-up` runs the same `colima start` for you. colima's virtiofs
share only covers `$HOME` by default — real task workdirs (`~/Kahya`,
`~/Library/Application Support/Kahya`) are always under it; a bind mount
OUTSIDE it silently gets an empty, root-owned, unwritable dir, not an error.

## Docker Desktop (fallback)

Install, launch once, confirm `docker info` succeeds — `make
sandbox-image` then works identically either way.

## Build the sandbox image

```sh
make sandbox-image   # builds kahya-sandbox:<version>, pins docker/sandbox/IMAGE_DIGEST
```

`shell_docker` refuses to run until this has completed once (fail-closed).
