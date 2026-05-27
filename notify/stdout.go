package notify

import (
	"context"
	"fmt"
	"os"

	"myFirstGo/trading/signal"
)

type Stdout struct{}

func (Stdout) Notify(_ context.Context, sig signal.Signal, ctxInfo signal.Context) error {
	_, err := fmt.Fprintln(os.Stdout, formatAlert(sig, ctxInfo))
	return err
}
