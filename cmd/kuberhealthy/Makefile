# IMAGE="kuberhealthy/kuberhealthy"
# TAG="2.2.0"

build:
	docker build -t reg-dhc.app.corpintra.net/caas/kuberhealthy:v2.2.1-debug -f Dockerfile ../../

push:
	docker push reg-dhc.app.corpintra.net/caas/kuberhealthy:v2.2.1-debug

test:
	POD_NAME="kuberhealthy-test" go test -run TestWebServer -v -args -- --debug --forceMaster
run:
	go build
	POD_NAME="kuberhealthy-test" ./kuberhealthy --debug --forceMaster
run-influx:
	go build
	POD_NAME="kuberhealthy-test" ./kuberhealthy -debug -forceMaster -enableInflux -influxUrl=http://localhost:8086 -influxDB=kuberhealthy
