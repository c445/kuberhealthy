module github.com/comcast/kuberhealthy/cmd/kuberhealthy

go 1.14

replace github.com/go-resty/resty => gopkg.in/resty.v1 v1.10.0

replace google.golang.org/cloud => cloud.google.com/go v0.37.0

replace github.com/Sirupsen/logrus => github.com/sirupsen/logrus v1.3.0

require (
	github.com/Comcast/kuberhealthy/v2 v2.2.0
	github.com/Pallinder/go-randomdata v1.2.0
	github.com/ghodss/yaml v1.0.0
	github.com/google/uuid v1.1.1
	github.com/integrii/flaggy v1.4.4
	github.com/sirupsen/logrus v1.6.0
	k8s.io/api v0.0.0-20190819141258-3544db3b9e44
	k8s.io/apimachinery v0.0.0-20190817020851-f2f3a405f61d
	k8s.io/client-go v0.0.0-20190819141724-e14f31a72a77
	k8s.io/klog v1.0.0
)
