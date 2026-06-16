# ocr-sidecar

Stateless OCR sidecar for CoverOnes KYC ID-card verification. Wraps Tesseract
(`chi_tra` + `eng`) and exposes a single endpoint:

```
POST /ocr   (multipart "image" field OR raw image body; JPEG/PNG, ≤ 8 MB)
→ {"name": "<chinese name>", "nationalId": "<TW id>", "confidence": <0-100>}
```

## Privacy

The uploaded image is processed in bounded memory and **discarded immediately** —
the only on-disk artifact is a temporary file (in `os.TempDir()`) required by the
Tesseract CLI, deleted via `defer` after the process exits. Nothing is persisted.

## Run

- `OCR_PORT` (default `8085`)
- Health: `GET /healthz` (and `/server -healthcheck` for container probes)

Consumed by the `kyc` service via `KYC_OCR_PROVIDER=http` + `KYC_OCR_URL`.
Production OCR provider (Azure / FaceMe) is pluggable behind the same contract.
