// hooks/useInstances.ts — Instance list with polling for transitional states.
//
// Polls every 5 seconds when any instance is in a transitional state.
// Source: 09-02 §Mechanism: Asynchronous Polling, M7 spec §Polling.

import { useCallback, useEffect, useRef, useState } from 'react';
import { instancesApi } from '../api/client';
import type { Instance } from '../types';
import { ApiException } from '../types';
import { isTransitional } from '../utils/states';

const POLL_INTERVAL_MS = 5000;

interface UseInstancesResult {
  instances: Instance[];
  loading: boolean;
  error: ApiException | null;
  refresh: () => void;
}

export function useInstances(): UseInstancesResult {
  const [instances, setInstances] = useState<Instance[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<ApiException | null>(null);
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const mountedRef = useRef(true);

  const load = useCallback(async (isInitial = false) => {
    if (isInitial) setLoading(true);
    try {
      const res = await instancesApi.list();
      if (!mountedRef.current) return;
      setInstances(res.instances ?? []);
      setError(null);

      // Schedule next poll if any instance is in a transitional state.
      const needsPoll = (res.instances ?? []).some((i) => isTransitional(i.status));
      if (needsPoll) {
        timerRef.current = setTimeout(() => load(), POLL_INTERVAL_MS);
      }
    } catch (err) {
      if (!mountedRef.current) return;
      if (err instanceof ApiException) {
        setError(err);
      } else {
        setError(new ApiException(0, {
          error: {
            code: 'network_error',
            message: 'Network error — could not reach the API.',
            request_id: '',
            details: [],
          },
        }));
      }
    } finally {
      if (isInitial && mountedRef.current) setLoading(false);
    }
  }, []);

  useEffect(() => {
    mountedRef.current = true;
    load(true);
    return () => {
      mountedRef.current = false;
      if (timerRef.current) clearTimeout(timerRef.current);
    };
  }, [load]);

  const refresh = useCallback(() => {
    if (timerRef.current) clearTimeout(timerRef.current);
    load();
  }, [load]);

  return { instances, loading, error, refresh };
}
