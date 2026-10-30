.PHONY: package-images proto smoke

proto:
	PROTO_PYTHON="$${PROTO_PYTHON:-python3}" bash scripts/generate-proto.sh

package-images:
	bash scripts/package-images.sh

# 逐角色做构造级启动自检：打开数据库并迁移、读配置、建客户端，然后退出 0。
# 需要 ALERTFLOW_DATABASE_URL、ALERTFLOW_KAFKA_BROKERS（构造期只校验非空，不连 broker）；
# server 角色只构造 HTTP/gRPC 栈，不监听端口。
smoke:
	cd backend && for role in server monitor publisher consumer delivery retire-deliveries; do \
		echo "== $$role"; \
		ALERTFLOW_ROLE=$$role go run ./cmd/alertflow -smoke || exit 1; \
	done
