build:
	cd cmd/karmada-extend/ && go build
	

buildpush:
	docker build -t ghcr.io/liangyuanpeng/karmada-extend:v0.0.1 .
	docker push ghcr.io/liangyuanpeng/karmada-extend:v0.0.1
