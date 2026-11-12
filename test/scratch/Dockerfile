# Runs the static binary in an empty image: no libc, no shell, no CA bundle.
# Build the binary first (see scripts/scratch-smoke.sh).
FROM scratch
COPY hivemind /hivemind
WORKDIR /
ENTRYPOINT ["/hivemind"]
CMD ["version"]
