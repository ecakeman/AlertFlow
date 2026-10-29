.PHONY: package-images proto

proto:
	PROTO_PYTHON="$${PROTO_PYTHON:-python3}" bash scripts/generate-proto.sh

package-images:
	bash scripts/package-images.sh
