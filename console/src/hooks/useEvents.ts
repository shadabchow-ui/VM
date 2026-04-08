// hooks/useEvents.ts — Instance event history.
// Source: EVENTS_SCHEMA_V1, 09-01 §Event History Card.

import { useCallback, useEffect, useState } from 'react';
import { instancesApi } from '../api/client';
import type { InstanceEvent } from '../types';
import { ApiException } from '../types';

interface UseEventsResult {
  events: InstanceEvent[];
  loading: boolean;
  error: ApiException | null;
}

export function useEvents(instanceId: string): UseEventsResult {
  const [events, setEvents] = useState<InstanceEvent[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<ApiException | null>(null);

  const load = useCallback(async () => {
    if (!instanceId) return;
    setLoading(true);
    try {
      const res = await instancesApi.listEvents(instanceId);
      setEvents(res.events ?? []);
      setError(null);
    } catch (err) {
      setError(err instanceof ApiException ? err : null);
    } finally {
      setLoading(false);
    }
  }, [instanceId]);

  useEffect(() => { load(); }, [load]);

  return { events, loading, error };
}
