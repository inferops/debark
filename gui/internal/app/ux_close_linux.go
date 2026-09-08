//go:build linux && cgo

package app

/*
#cgo pkg-config: gtk+-3.0
#include <gtk/gtk.h>
#include <pthread.h>

typedef struct {
    pthread_mutex_t mutex;
    pthread_cond_t condition;
    int active_job;
    int finished;
    int accepted;
} debark_close_request;

static gboolean debark_close_on_main(gpointer data) {
    debark_close_request *request = data;
    GtkWindow *parent = NULL;
    GList *windows = gtk_window_list_toplevels();
    for (GList *item = windows; item != NULL; item = item->next) {
        GtkWindow *window = GTK_WINDOW(item->data);
        const gchar *title = gtk_window_get_title(window);
        if (g_strcmp0(title, "Debark") == 0) {
            parent = g_object_ref(window);
            break;
        }
    }
    g_list_free(windows);
    const char *message = request->active_job
        ? "Stop the running operation and close Debark? An interrupted copy will remain marked incomplete."
        : "Close Debark? Your package selection has not produced a complete bundle and will be lost.";
    GtkWidget *dialog = gtk_message_dialog_new(parent,
        GTK_DIALOG_MODAL | GTK_DIALOG_DESTROY_WITH_PARENT,
        GTK_MESSAGE_QUESTION, GTK_BUTTONS_NONE, "%s", message);
    g_object_ref_sink(dialog);
    gtk_window_set_title(GTK_WINDOW(dialog), "Close Debark");
    gtk_dialog_add_buttons(GTK_DIALOG(dialog), "Keep working", GTK_RESPONSE_CANCEL,
        request->active_job ? "Stop and close" : "Close without saving", GTK_RESPONSE_ACCEPT, NULL);
    gtk_dialog_set_default_response(GTK_DIALOG(dialog), GTK_RESPONSE_CANCEL);
    int accepted = gtk_dialog_run(GTK_DIALOG(dialog)) == GTK_RESPONSE_ACCEPT;
    gtk_widget_destroy(dialog);
    g_object_unref(dialog);
    if (parent != NULL) g_object_unref(parent);
    pthread_mutex_lock(&request->mutex);
    request->accepted = accepted;
    request->finished = 1;
    pthread_cond_signal(&request->condition);
    pthread_mutex_unlock(&request->mutex);
    return G_SOURCE_REMOVE;
}

static int debark_confirm_close(int active_job) {
    debark_close_request request = {
        .mutex = PTHREAD_MUTEX_INITIALIZER,
        .condition = PTHREAD_COND_INITIALIZER,
        .active_job = active_job,
    };
    pthread_mutex_lock(&request.mutex);
    // The C-owned request remains alive until the UI callback has finished.
    // An idle source always dispatches on GTK's main loop, unlike invoke,
    // which may run inline when the caller can acquire the default context.
    g_idle_add_full(G_PRIORITY_DEFAULT, debark_close_on_main, &request, NULL);
    while (!request.finished) pthread_cond_wait(&request.condition, &request.mutex);
    int accepted = request.accepted;
    pthread_mutex_unlock(&request.mutex);
    pthread_cond_destroy(&request.condition);
    pthread_mutex_destroy(&request.mutex);
    return accepted;
}
*/
import "C"

import "context"

// Wails v2.12.0 internal/frontend/desktop/linux/window.go:MessageDialog
// discards Buttons, DefaultButton and CancelButton. Its window.c uses fixed
// Yes/No buttons. This narrow GTK adapter preserves explicit destructive
// wording and Keep working as the default without modifying Wails.
// Called only by runClose's worker goroutine, never by the GTK thread.
func nativeConfirmClose(_ context.Context, activeJob bool) (bool, error) {
	active := 0
	if activeJob {
		active = 1
	}
	return C.debark_confirm_close(C.int(active)) != 0, nil
}
