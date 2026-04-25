#!/usr/bin/env python3

import hashlib
import json
import pathlib
import sys

import numpy as np
from pydantic import BaseModel


class Sample(BaseModel):
    shard: int
    index: int
    payload: list[float]


def main() -> None:
    shard = int(sys.argv[1]) if len(sys.argv) > 1 else 0
    document_count = int(sys.argv[2]) if len(sys.argv) > 2 else 180
    vector_size = int(sys.argv[3]) if len(sys.argv) > 3 else 3000

    out_dir = pathlib.Path("tmp") / f"python-shard-{shard}"
    out_dir.mkdir(parents=True, exist_ok=True)

    checksum = hashlib.sha256()
    for index in range(document_count):
        vector = np.linspace(index, index + 1, vector_size, dtype=np.float64)
        sample = Sample(shard=shard, index=index, payload=vector[:12].tolist())
        checksum.update(sample.model_dump_json().encode("utf-8"))
        centered = vector - vector.mean()
        score = float(np.dot(centered, centered))
        record = {
            "index": index,
            "score": score,
            "sample": sample.model_dump(),
        }
        (out_dir / f"sample-{index}.json").write_text(json.dumps(record))

    (out_dir / "summary.json").write_text(
        json.dumps(
            {
                "shard": shard,
                "documents": document_count,
                "vector_size": vector_size,
                "checksum": checksum.hexdigest(),
            },
            indent=2,
        )
    )

    print(f"python workload complete for shard {shard}")


if __name__ == "__main__":
    main()
