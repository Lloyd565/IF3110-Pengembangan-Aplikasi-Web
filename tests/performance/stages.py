import json
import sys

STAGES = {"baseline": 60, "stress3": 60, "stress6": 60, "recovery": 60, "rampdown": 30}


def main(path):
    m = json.load(open(path, encoding="utf-8"))["metrics"]

    def get(name, stat):
        return m.get(name, {}).get("values", {}).get(stat)

    def fmt(v, digits=1):
        return "-" if v is None else f"{v:.{digits}f}"

    print("| Tahap | API p50 (ms) | API p95 (ms) | Request/s | Error rate | Queue wait p50 / p95 (ms) | Durasi job p50 / p95 (ms) | Job selesai | Job/s |")
    print("|---|---:|---:|---:|---:|---:|---:|---:|---:|")
    for stage, seconds in STAGES.items():
        api = f"http_req_duration{{scenario:api,stage:{stage}}}"
        wait, dur = f"job_queue_wait_ms{{stage:{stage}}}", f"job_duration_ms{{stage:{stage}}}"
        reqs = get(f"http_reqs{{stage:{stage}}}", "count")
        jobs = get(f"jobs_completed{{stage:{stage}}}", "count")
        print(f"| {stage} | {fmt(get(api, 'med'))} | {fmt(get(api, 'p(95)'))} | {fmt(reqs and reqs / seconds)} | "
              f"{fmt(get(f'http_req_failed{{stage:{stage}}}', 'rate'), 4)} | {fmt(get(wait, 'med'), 0)} / {fmt(get(wait, 'p(95)'), 0)} | "
              f"{fmt(get(dur, 'med'), 0)} / {fmt(get(dur, 'p(95)'), 0)} | {fmt(jobs, 0)} | {fmt(jobs and jobs / seconds, 2)} |")


if __name__ == "__main__":
    main(sys.argv[1])
