import { useCallback, useEffect, useRef, useState } from "react";
import { errorMessage, get } from "./api";

export interface FetchState<T> {
  data: T | undefined;
  error: string | undefined;
  loading: boolean;
  reload: () => void;
  setData: (fn: (prev: T | undefined) => T | undefined) => void;
}

/**
 * GET a JSON resource. Data from a previous path is dropped when the path
 * changes; reloads keep the current data visible (no flashing).
 */
export function useFetch<T>(path: string | null): FetchState<T> {
  const [state, setState] = useState<{ key: string | null; data?: T; error?: string; loading: boolean }>({
    key: path,
    loading: !!path,
  });
  const [tick, setTick] = useState(0);

  useEffect(() => {
    if (!path) return;
    let cancelled = false;
    setState((s) => (s.key === path ? { ...s, loading: true } : { key: path, loading: true }));
    get<T>(path).then(
      (data) => {
        if (!cancelled) setState({ key: path, data, loading: false });
      },
      (e) => {
        if (!cancelled)
          setState((s) => ({ key: path, data: s.key === path ? s.data : undefined, error: errorMessage(e), loading: false }));
      },
    );
    return () => {
      cancelled = true;
    };
  }, [path, tick]);

  const reload = useCallback(() => setTick((t) => t + 1), []);
  const setData = useCallback(
    (fn: (prev: T | undefined) => T | undefined) => setState((s) => ({ ...s, data: fn(s.data) })),
    [],
  );

  const current = state.key === path;
  return {
    data: current ? state.data : undefined,
    error: current ? state.error : undefined,
    loading: current ? state.loading : !!path,
    reload,
    setData,
  };
}

type Handlers = Record<string, (data: unknown) => void>;

/**
 * Subscribe to a server-sent-events stream. Handlers may change between
 * renders without reconnecting. `onOpen` fires on every (re)connect so the
 * caller can refetch anything missed while disconnected.
 */
export function useEventSource(url: string | null, handlers: Handlers, onOpen?: () => void) {
  const handlersRef = useRef(handlers);
  handlersRef.current = handlers;
  const openRef = useRef(onOpen);
  openRef.current = onOpen;
  const names = Object.keys(handlers).sort().join(",");

  useEffect(() => {
    if (!url) return;
    let es: EventSource | null = null;
    let retry: ReturnType<typeof setTimeout> | undefined;
    let closed = false;
    let first = true;
    const connect = () => {
      es = new EventSource(url);
      es.onopen = () => {
        if (!first) openRef.current?.();
        first = false;
      };
      for (const name of names.split(",").filter(Boolean)) {
        es.addEventListener(name, (ev) => {
          let data: unknown = null;
          try {
            data = JSON.parse((ev as MessageEvent).data);
          } catch {
            /* ignore */
          }
          handlersRef.current[name]?.(data);
        });
      }
      es.onerror = () => {
        // Browsers retry transient errors by themselves; a CLOSED source
        // (e.g. non-200 response) needs a manual retry.
        if (es && es.readyState === EventSource.CLOSED && !closed) {
          first = false;
          retry = setTimeout(connect, 5000);
        }
      };
    };
    connect();
    return () => {
      closed = true;
      clearTimeout(retry);
      es?.close();
    };
  }, [url, names]);
}

/** Current time, refreshed every `ms` while `active`. */
export function useNow(active: boolean, ms = 1000): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!active) return;
    setNow(Date.now());
    const t = setInterval(() => setNow(Date.now()), ms);
    return () => clearInterval(t);
  }, [active, ms]);
  return active ? now : Date.now();
}

/** Returns a function that calls `fn` at most once per `ms` (trailing). */
export function useThrottle(fn: () => void, ms = 500): () => void {
  const fnRef = useRef(fn);
  fnRef.current = fn;
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);
  useEffect(() => () => {
    if (timer.current) clearTimeout(timer.current);
  }, []);
  return useCallback(() => {
    if (timer.current) return;
    timer.current = setTimeout(() => {
      timer.current = null;
      fnRef.current();
    }, ms);
  }, [ms]);
}

export function useDocumentTitle(title: string | undefined) {
  useEffect(() => {
    document.title = title ? `${title} · kiwicd` : "kiwicd";
  }, [title]);
}
