/*
 * bus.js — the subscription tracker behind `ctx.on` / `ctx.off`.
 *
 * Why this file exists
 * --------------------
 * The backend emits eighteen events. If every screen called
 * `runtime.EventsOn` for the ones it cares about, then every screen would also
 * have to remember to call `EventsOff` for exactly those, in `destroy`, without
 * missing one. Nobody manages that ten screens in a row. The listener that gets
 * missed does not fail loudly: it keeps firing, against a DOM that was thrown
 * away, and the symptom shows up three screens later as a mystery.
 *
 * So the shell subscribes to the Wails runtime exactly once per event name and
 * fans out from here. A screen never receives a handle to the runtime at all —
 * `ctx` has no `runtime`, no `EventsOn`, no unsubscribe token it could drop.
 * The only way for a screen to hear an event is `ctx.on`, and `ctx.on` is bound
 * to that screen's own scope, which the shell owns and disposes.
 *
 * Seven properties make a leak structurally impossible rather than a matter of
 * discipline:
 *
 *   1. The runtime handle is private to this module. A screen cannot subscribe
 *      behind the shell's back because it is never given the thing to do it
 *      with.
 *   2. Every `scope.on` is recorded in that scope's own ledger at the moment it
 *      is made. There is no path that registers a handler without recording it:
 *      registration *is* the recording.
 *   3. `scope.dispose()` removes every recorded handler. The shell calls it in
 *      a `finally` around each screen's `destroy()`, so a screen whose
 *      `destroy` throws — or which has no `destroy` at all — still gets its
 *      listeners removed.
 *   4. A disposed scope is sealed. A later `on()` from a stray timer or an
 *      in-flight promise is a warned no-op, so a dead screen cannot resurrect
 *      a listener after teardown.
 *   5. Dispatch iterates a snapshot of the handler set, so a handler that
 *      unsubscribes itself (or its whole scope) mid-dispatch cannot corrupt the
 *      iteration.
 *   6. A handler that throws is caught and reported. One screen's bad handler
 *      cannot stop the fan-out to the others, which would otherwise look
 *      exactly like a leak.
 *   7. The shell's own listeners live in a scope too. There is no privileged
 *      path that skips the ledger.
 *
 * This module knows nothing about Wails. It is handed a `subscribe` function
 * and is otherwise pure, which is what makes it testable without a webview.
 */

const NOOP = () => {};

/**
 * createEventBus wires one runtime subscription per event name and fans out to
 * scoped subscribers.
 *
 * @param {object} opts
 * @param {string[]} opts.names        Event names to attach eagerly. The
 *                                     eighteen from the frozen binding surface.
 * @param {(name: string, cb: (...data: any[]) => void) => (undefined|Function)}
 *        opts.subscribe               Attaches one runtime listener. May return
 *                                     an unsubscribe function; if it does not,
 *                                     `unsubscribeAll` is used instead.
 * @param {(name: string) => void} [opts.unsubscribe]
 *                                     Detaches by name, for runtimes whose
 *                                     subscribe returns nothing.
 * @param {(err: any, context: string) => void} [opts.onError]
 *                                     Reports a throwing handler or a failed
 *                                     attach. Never used for control flow.
 */
export function createEventBus({ names = [], subscribe, unsubscribe, onError } = {}) {
  if (typeof subscribe !== 'function') {
    throw new TypeError('createEventBus: subscribe must be a function');
  }
  const report = typeof onError === 'function' ? onError : NOOP;

  /** @type {Map<string, Set<Function>>} name -> handlers */
  const channels = new Map();
  /** @type {Map<string, Function>} name -> runtime detach */
  const detach = new Map();
  /** @type {Set<object>} live scopes, so closing the bus closes them all */
  const scopes = new Set();

  const known = new Set(names);
  let closed = false;
  let seq = 0;

  function dispatch(name, payload) {
    const set = channels.get(name);
    if (!set || set.size === 0) return;
    // Snapshot: a handler is allowed to unsubscribe itself, or its whole
    // screen, from inside the callback.
    for (const handler of Array.from(set)) {
      try {
        handler(payload, name);
      } catch (err) {
        report(err, `handler for "${name}"`);
      }
    }
  }

  function attach(name) {
    if (channels.has(name)) return channels.get(name);
    const set = new Set();
    channels.set(name, set);
    try {
      const off = subscribe(name, (...data) => {
        // The backend emits exactly one payload per event. Wails spreads the
        // emitted arguments, so a single payload arrives as data[0]. Anything
        // else is handed over verbatim rather than silently discarded.
        dispatch(name, data.length > 1 ? data : data[0]);
      });
      if (typeof off === 'function') {
        detach.set(name, off);
      } else if (typeof unsubscribe === 'function') {
        detach.set(name, () => unsubscribe(name));
      }
    } catch (err) {
      report(err, `subscribing to "${name}"`);
    }
    return set;
  }

  for (const name of known) attach(name);

  function add(name, handler) {
    if (typeof handler !== 'function') {
      report(new TypeError('handler must be a function'), `on("${name}")`);
      return false;
    }
    if (!known.has(name)) {
      // Not fatal — the backend may have grown an event this frontend build
      // predates — but almost always a typo, and a typo here is a subscription
      // that never fires and never explains itself.
      report(
        new Error(`unknown event name "${name}"; expected one of: ${[...known].join(', ')}`),
        'subscription',
      );
    }
    attach(name).add(handler);
    return true;
  }

  function remove(name, handler) {
    const set = channels.get(name);
    if (set) set.delete(handler);
  }

  /**
   * scope creates a per-screen subscription ledger. Everything registered
   * through it is removed by a single `dispose()`.
   *
   * @param {string} label  For diagnostics only — the screen id.
   */
  function scope(label = `scope-${++seq}`) {
    /** @type {Map<string, Set<Function>>} */
    const ledger = new Map();
    let disposed = false;

    const self = {
      label,
      get disposed() {
        return disposed;
      },

      /**
       * on subscribes for the lifetime of this scope.
       * @returns {Function} an unsubscribe, for the rare screen that wants to
       *          drop a listener early. Ignoring it is safe: dispose covers it.
       */
      on(name, handler) {
        if (disposed || closed) {
          report(
            new Error(`"${label}" subscribed to "${name}" after teardown; ignored`),
            'late subscription',
          );
          return NOOP;
        }
        if (!add(name, handler)) return NOOP;
        let set = ledger.get(name);
        if (!set) {
          set = new Set();
          ledger.set(name, set);
        }
        set.add(handler);
        return () => self.off(name, handler);
      },

      off(name, handler) {
        const set = ledger.get(name);
        if (set) {
          set.delete(handler);
          if (set.size === 0) ledger.delete(name);
        }
        remove(name, handler);
      },

      /** size is the number of live subscriptions. A test asserts it is 0. */
      size() {
        let n = 0;
        for (const set of ledger.values()) n += set.size;
        return n;
      },

      dispose() {
        if (disposed) return 0;
        disposed = true;
        let n = 0;
        for (const [name, set] of ledger) {
          for (const handler of set) {
            remove(name, handler);
            n += 1;
          }
        }
        ledger.clear();
        scopes.delete(self);
        return n;
      },
    };

    scopes.add(self);
    return self;
  }

  return {
    scope,

    /** names attached at construction, for diagnostics and tests. */
    names: () => [...known],

    /** count reports live subscriptions across every scope. Tests assert 0. */
    count() {
      let n = 0;
      for (const set of channels.values()) n += set.size;
      return n;
    },

    /**
     * emit delivers a payload as if it came from the runtime. It exists for
     * the shell's own synthetic notices and for tests; nothing in a screen
     * should call it.
     */
    emit(name, payload) {
      dispatch(name, payload);
    },

    /** close disposes every scope and detaches every runtime listener. */
    close() {
      if (closed) return;
      closed = true;
      for (const s of Array.from(scopes)) s.dispose();
      for (const [name, off] of detach) {
        try {
          off();
        } catch (err) {
          report(err, `unsubscribing "${name}"`);
        }
      }
      detach.clear();
      channels.clear();
      scopes.clear();
    },
  };
}
