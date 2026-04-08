// hooks/useInstance.ts — Single instance with polling during transitional states.
//
// Source: 09-02 §Provisioning Progress, M7 spec §Polling.

import { useCallback, useEffect, useRef, useState } from 'react';
import { instancesApi } from '../api/client';
import type { Instance } from '../types';
import { ApiException } from '../types';
import { isTransitional } from '../utils/states';

const POLL_INTERVAL_MS = 5000;

interface UseInstanceResult {
  instance: Instance | null;
  loading: boolean;
  error: ApiException | null;
  refresh: () => void;
}

export function useInstance(id: string): UseInstanceResult {
  const [instance, setInstance] = useState<Instance | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<ApiException | null>(null);
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const mountedRef = useRef(true);

  const load = useCallback(async (isInitial = false) => {
    if (isInitial) setLoading(true);
    try {
      const inst = await instancesApi.get(id);
      if (!mountedRef.current) return;
      setInstance(inst);
      setError(null);

      // Keep polling while transitional.
      if (isTransitional(inst.status)) {
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
  }, [id]);

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

  return { instance, loading, error, refresh };
}
