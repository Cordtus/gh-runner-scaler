# runner-load-lab

Synthetic workload repo for exercising `gh-runner-scaler` with mixed
GitHub Actions workloads on self-hosted runners.

The workflows are intentionally designed to stress:

- queue bursts with many short jobs
- Node/TypeScript dependency and bundling work
- Python dependency and CPU work
- Go test/build loops
- Playwright browser execution
