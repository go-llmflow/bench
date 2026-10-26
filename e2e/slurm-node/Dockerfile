# A single-node Slurm 24.11 with slurmrestd (auth/jwt), for
# TestSlurmDiscoveryE2E. Build: docker build -t llmflow-slurm-e2e e2e/slurm-node
FROM debian:trixie
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      slurmctld slurmd slurmrestd slurm-client slurm-wlm-jwt-plugin munge curl ca-certificates procps && \
    rm -rf /var/lib/apt/lists/*
RUN useradd -m svc_llm && useradd -m alice && useradd -r restd
COPY slurm.conf /etc/slurm/slurm.conf
COPY cgroup.conf /etc/slurm/cgroup.conf
COPY start.sh /start.sh
RUN chmod +x /start.sh
CMD ["/start.sh"]
