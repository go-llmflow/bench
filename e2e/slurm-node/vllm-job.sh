#!/bin/sh
# The Slurm job of the E2E: a vLLM CPU instance on port $1, its API key
# read from file $2 into VLLM_API_KEY (never on the command line, which
# every user of the node can read in the process list).
export HF_HOME=/hf HF_HUB_OFFLINE=1 VLLM_CPU_KVCACHE_SPACE=1
# sbatch exports the submitter's environment: HOME would be root's.
export HOME=/tmp/llmflow-vllm-home
mkdir -p "$HOME"
VLLM_API_KEY=$(cat "$2")
export VLLM_API_KEY
exec vllm serve Qwen/Qwen2.5-0.5B-Instruct --host 0.0.0.0 --port "$1" --max-model-len 2048 --enforce-eager
