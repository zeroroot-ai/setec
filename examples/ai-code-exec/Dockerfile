# SPDX-License-Identifier: Apache-2.0
#
# Minimal Python image for the ai-code-exec example workload. The client
# program pipes source code via `python3 -c`; this image exists so users
# can pin a known Python version rather than pulling from Docker Hub at
# launch time.

FROM python:3.12-slim@sha256:876416ecde9aca2bcc90e1fb0c7a9500bbf749f5788b70f82d4c5a5c2357f8b4

# Harden: run as a non-root user inside the VM.
RUN useradd --create-home --shell /usr/sbin/nologin runner
USER runner
WORKDIR /home/runner

# No ENTRYPOINT on purpose. The Sandbox spec supplies the command.
CMD ["python3", "--version"]
