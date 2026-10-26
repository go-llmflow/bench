#!/bin/sh
# Single-node Slurm with slurmrestd (auth/jwt), for the gateway's E2E.
set -e
mkdir -p /var/spool/slurmctld /var/spool/slurmd /run/munge
chown slurm /var/spool/slurmctld
[ -f /etc/munge/munge.key ] || mungekey -c
chown -R munge /etc/munge /run/munge /var/lib/munge /var/log/munge
su -s /bin/sh munge -c "munged"
dd if=/dev/urandom bs=32 count=1 of=/var/spool/slurmctld/jwt_hs256.key 2>/dev/null
chown slurm /var/spool/slurmctld/jwt_hs256.key
chmod 0600 /var/spool/slurmctld/jwt_hs256.key
slurmctld
slurmd
sleep 2
su -s /bin/sh restd -c "SLURM_JWT=daemon slurmrestd -a rest_auth/jwt -s openapi/slurmctld 0.0.0.0:6820 >/tmp/slurmrestd.log 2>&1 &"
exec sleep infinity
