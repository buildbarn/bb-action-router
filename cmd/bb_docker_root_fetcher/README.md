### bb_docker_root_fetcher
The main purpose of this component is to share pulled images across runners and amortize the cost of materializing
the docker image contents. This is especially important if the images are large and FUSE is not used.

The fetcher runs as a sidecar container alongside the worker. It listens on a Unix domain socket for image requests from
the helper. On a cache miss, it:
1. Pulls the Docker image from the container registry,
2. Extracts layers and uploads file content to a local ephemeral CAS,
3. Materializes the directory tree by hardlinking files from the CAS into a root directory.

On a cache hit, the fetcher returns the existing materialized path immediately.

The helper communicates with the fetcher via a simple line-based protocol over a Unix domain socket (we didn't want
to add C++ gRPC to the helper). The fetcher will then watch the PID of the helper to know when the files are no longer
being used (this avoids the need for an explicit release message).