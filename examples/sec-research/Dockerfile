# SPDX-License-Identifier: Apache-2.0
#
# AFL++ base image for the sec-research example. The client injects the
# target binary and seed corpus at launch time; this image provides the
# afl-fuzz toolchain and the coreutils the harness script needs.

FROM docker.io/aflplusplus/aflplusplus:latest@sha256:2d509634e426f0bd7059bf2b21f0753e8630228ede6f7e7b2032213c3a3c5cc3

# The harness extracts the bundle into /work.
WORKDIR /work

CMD ["afl-fuzz", "-h"]
