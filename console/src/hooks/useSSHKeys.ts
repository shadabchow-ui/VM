// hooks/useSSHKeys.ts — SSH key list with refresh.

import { useCallback, useEffect, useState } from 'react';
import { sshKeysApi } from '../api/client';
import type { SSHKey } from '../types';
import { ApiException } from '../types';

interface UseSSHKeysResult {
  keys: SSHKey[];
  loading: boolean;
  error: ApiException | null;
  refresh: () => void;
}

export function useSSHKeys(): UseSSHKeysResult {
  const [keys, setKeys] = useState<SSHKey[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<ApiException | null>(null);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const res = await sshKeysApi.list();
      setKeys(res.ssh_keys ?? []);
      setError(null);
    } catch (err) {
      setError(err instanceof ApiException ? err : null);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => { load(); }, [load]);

  return { keys, loading, error, refresh: load };
}
