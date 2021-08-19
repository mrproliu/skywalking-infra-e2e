module github.com/apache/skywalking-infra-e2e

go 1.16

require (
	github.com/docker/docker v20.10.8+incompatible
	github.com/docker/go-connections v0.4.0
	github.com/google/go-cmp v0.5.4
	github.com/gorilla/mux v1.8.0 // indirect
	github.com/morikuni/aec v1.0.0 // indirect
	github.com/mrproliu/testcontainers-go v0.11.2-0.20210819035138-8e122b12124f
	github.com/sirupsen/logrus v1.7.0
	github.com/spf13/cobra v1.1.1
	gopkg.in/yaml.v2 v2.4.0
	k8s.io/api v0.20.7
	k8s.io/apimachinery v0.20.7
	k8s.io/cli-runtime v0.20.7
	k8s.io/client-go v0.20.7
	k8s.io/kubectl v0.20.7
	sigs.k8s.io/kind v0.9.0
)
