package main

import (
	"os"

	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	"k8s.io/component-base/cli"
	controllerruntime "sigs.k8s.io/controller-runtime"

	"github.com/liangyuanpeng/karmada-extend/cmd/karmada-extend/app"
)

func main() {
	stopChan := controllerruntime.SetupSignalHandler().Done()
	command := app.NewKarmadaExtendCommand(stopChan)
	code := cli.Run(command)
	os.Exit(code)
}
