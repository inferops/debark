//go:build !linux || !cgo

package app

import (
	"context"
	"fmt"
	"runtime"

	wruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

func nativeConfirmClose(ctx context.Context, activeJob bool) (bool, error) {
	if runtime.GOOS == "linux" {
		return false, fmt.Errorf("native close confirmation requires a GTK desktop build")
	}
	action := "Close without saving"
	message := "Close Debark? Your package selection has not produced a complete bundle and will be lost."
	if activeJob {
		action = "Stop and close"
		message = "Stop the running operation and close Debark? An interrupted copy will remain marked incomplete."
	}
	choice, err := wruntime.MessageDialog(ctx, wruntime.MessageDialogOptions{
		Type: wruntime.QuestionDialog, Title: "Close Debark", Message: message,
		Buttons: []string{"Keep working", action}, DefaultButton: "Keep working", CancelButton: "Keep working",
	})
	return choice == action && err == nil, err
}
