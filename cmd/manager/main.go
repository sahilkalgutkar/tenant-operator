// Command manager runs the tenant operator.
package main

import (
	"fmt"
	"os"

	"go.uber.org/zap/zapcore"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/sahilkalgutkar/tenant-operator/internal/bootstrap"
)

func main() {
	opts, err := bootstrap.ParseFlags(os.Args[1:], os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	ctrl.SetLogger(zap.New(zap.UseDevMode(false), zap.Level(zapcore.InfoLevel)))

	if err := bootstrap.Run(ctrl.SetupSignalHandler(), opts); err != nil {
		ctrl.Log.Error(err, "the manager exited with an error")
		os.Exit(1)
	}
}
