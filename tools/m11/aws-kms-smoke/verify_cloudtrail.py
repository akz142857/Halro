#!/usr/bin/env python3
import json
import sys


def events(path):
    with open(path, encoding="utf-8") as source:
        payload = json.load(source)
    for outer in payload.get("Events", []):
        raw = outer.get("CloudTrailEvent")
        if not raw:
            continue
        try:
            yield json.loads(raw)
        except json.JSONDecodeError:
            continue


def correlate(private_path, encrypt_path, decrypt_path):
    with open(private_path, encoding="utf-8") as source:
        private = json.load(source)

    encrypt_ok = any(
        event.get("eventSource") == "kms.amazonaws.com"
        and event.get("eventName") == "Encrypt"
        and event.get("requestID") == private["encrypt_request_id"]
        for event in events(encrypt_path)
    )
    decrypt_ok = any(
        event.get("eventSource") == "kms.amazonaws.com"
        and event.get("eventName") == "Decrypt"
        and event.get("requestID") == private["decrypt_request_id"]
        for event in events(decrypt_path)
    )
    return {
        "cloudtrail_encrypt_correlated": encrypt_ok,
        "cloudtrail_decrypt_correlated": decrypt_ok,
    }


def main(argv):
    if len(argv) != 4:
        raise SystemExit("usage: verify_cloudtrail.py PRIVATE ENCRYPT_EVENTS DECRYPT_EVENTS")
    result = correlate(argv[1], argv[2], argv[3])
    print(json.dumps(result, separators=(",", ":")))
    return 0 if all(result.values()) else 1


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
