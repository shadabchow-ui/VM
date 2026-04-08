// pages/CreateInstancePage.tsx — Create instance wizard.
//
// M7 requirements satisfied:
//   ✓ Image selection
//   ✓ Instance type selection
//   ✓ Availability zone selection
//   ✓ SSH key selection (from /v1/ssh-keys)
//   ✓ Instance name with validation
//   ✓ Submits POST /v1/instances with Idempotency-Key header
//   ✓ Redirect to instances list on success (provisioning state visible immediately)
//   ✓ Inline field errors for 4xx with target field
//   ✓ 5xx generic message with request_id
//   ✓ Create button disabled until all required fields filled
//
// Source: 09-01 §Create Instance Wizard, M7 spec §B.

import React, { useEffect, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { instancesApi, sshKeysApi } from '../api/client';
import { Breadcrumb } from '../components/Layout';
import { Skeleton } from '../components/Skeleton';
import { useToast } from '../components/Toast';
import type { SSHKey } from '../types';
import { ApiException } from '../types';
import { generateIdempotencyKey } from '../utils/format';

// Phase 1 catalogs — derived from instance_validation.go.
// Source: INSTANCE_MODEL_V1 §6, §7, instance_validation.go.
const IMAGES = [
  { id: 'img_ubuntu2204', label: 'Ubuntu 22.04 LTS', sub: 'LTS · 5-year security updates' },
  { id: 'img_debian12',   label: 'Debian 12 (Bookworm)', sub: 'Stable · lean · widely compatible' },
];

const INSTANCE_TYPES = [
  { id: 'gp1.small',  label: 'gp1.small',  specs: '2 vCPU · 4 GB RAM · 50 GB disk' },
  { id: 'gp1.medium', label: 'gp1.medium', specs: '4 vCPU · 8 GB RAM · 100 GB disk' },
  { id: 'gp1.large',  label: 'gp1.large',  specs: '8 vCPU · 16 GB RAM · 200 GB disk' },
  { id: 'gp1.xlarge', label: 'gp1.xlarge', specs: '16 vCPU · 32 GB RAM · 500 GB disk' },
];

const ZONES = [
  { id: 'us-east-1a', label: 'us-east-1a' },
  { id: 'us-east-1b', label: 'us-east-1b' },
];

interface FormFields {
  name: string;
  image_id: string;
  instance_type: string;
  availability_zone: string;
  ssh_key_name: string;
}

type FieldErrors = Partial<Record<keyof FormFields | '_general', string>>;

export function CreateInstancePage() {
  const navigate = useNavigate();
  const { showToast } = useToast();

  const [sshKeys, setSshKeys] = useState<SSHKey[]>([]);
  const [keysLoading, setKeysLoading] = useState(true);
  const [submitting, setSubmitting] = useState(false);
  const [errors, setErrors] = useState<FieldErrors>({});

  const [form, setForm] = useState<FormFields>({
    name: '',
    image_id: IMAGES[0].id,
    instance_type: INSTANCE_TYPES[0].id,
    availability_zone: ZONES[0].id,
    ssh_key_name: '',
  });

  // Load existing SSH keys for selection.
  useEffect(() => {
    sshKeysApi.list()
      .then((res) => {
        const keys = res.ssh_keys ?? [];
        setSshKeys(keys);
        if (keys.length > 0) {
          setForm((f) => ({ ...f, ssh_key_name: keys[0].name }));
        }
      })
      .catch(() => {/* non-fatal — user sees empty dropdown */})
      .finally(() => setKeysLoading(false));
  }, []);

  function setField(field: keyof FormFields, value: string) {
    setForm((f) => ({ ...f, [field]: value }));
    setErrors((e) => ({ ...e, [field]: undefined }));
  }

  function validateClient(): FieldErrors {
    const e: FieldErrors = {};
    if (!form.name.trim()) {
      e.name = "Name is required.";
    } else if (!/^[a-z][a-z0-9-]{0,61}[a-z0-9]$/.test(form.name)) {
      e.name = "Must match ^[a-z][a-z0-9-]{0,61}[a-z0-9]$.";
    }
    if (!form.ssh_key_name) {
      e.ssh_key_name = "An SSH key is required.";
    }
    return e;
  }

  async function handleSubmit() {
    const clientErrs = validateClient();
    if (Object.keys(clientErrs).length > 0) {
      setErrors(clientErrs);
      return;
    }

    setSubmitting(true);
    setErrors({});

    try {
      await instancesApi.create(
        {
          name: form.name,
          image_id: form.image_id,
          instance_type: form.instance_type,
          availability_zone: form.availability_zone,
          ssh_key_name: form.ssh_key_name,
        },
        generateIdempotencyKey(),
      );
      showToast(`Instance '${form.name}' creation initiated`, 'success');
      navigate('/instances');
    } catch (err) {
      if (err instanceof ApiException) {
        if (err.isServerError) {
          setErrors({ _general: `An internal error occurred. (request_id: ${err.requestId})` });
        } else if (err.body.error.details?.length) {
          // Map per-field errors from 400 response details array.
          const fe: FieldErrors = {};
          for (const d of err.body.error.details) {
            if (d.target) {
              (fe as Record<string, string>)[d.target] = d.message;
            }
          }
          setErrors(fe);
        } else {
          setErrors({ _general: err.message });
        }
      }
    } finally {
      setSubmitting(false);
    }
  }

  const canSubmit =
    form.name.trim() !== '' &&
    form.image_id !== '' &&
    form.instance_type !== '' &&
    form.availability_zone !== '' &&
    form.ssh_key_name !== '' &&
    !submitting;

  return (
    <div style={{ padding: '32px 40px', maxWidth: 700 }}>
      <Breadcrumb
        items={[
          { label: 'Instances', path: '/instances' },
          { label: 'Create Instance' },
        ]}
      />

      <h1 style={{ margin: '0 0 6px', color: '#f3f4f6', fontSize: 21, fontWeight: 600 }}>
        Create Instance
      </h1>
      <p style={{ margin: '0 0 30px', color: '#6b7280', fontSize: 13 }}>
        Configure and launch a new virtual machine.
      </p>

      {/* General error banner */}
      {errors._general && (
        <div style={{
          padding: '12px 16px',
          borderRadius: 7,
          border: '1px solid rgba(248,113,113,0.28)',
          background: 'rgba(248,113,113,0.06)',
          color: '#f87171',
          fontSize: 13,
          marginBottom: 22,
        }}>
          {errors._general}
        </div>
      )}

      {/* ── Section: Image ── */}
      <FormSection label="Image">
        <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 10 }}>
          {IMAGES.map((img) => (
            <SelectCard
              key={img.id}
              selected={form.image_id === img.id}
              onClick={() => setField('image_id', img.id)}
              title={img.label}
              sub={img.sub}
            />
          ))}
        </div>
        <FieldErr msg={errors.image_id} />
      </FormSection>

      {/* ── Section: Instance Type ── */}
      <FormSection label="Plan">
        <div style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 10 }}>
          {INSTANCE_TYPES.map((t) => (
            <SelectCard
              key={t.id}
              selected={form.instance_type === t.id}
              onClick={() => setField('instance_type', t.id)}
              title={t.label}
              sub={t.specs}
            />
          ))}
        </div>
        <FieldErr msg={errors.instance_type} />
      </FormSection>

      {/* ── Section: Availability Zone ── */}
      <FormSection label="Availability Zone">
        <div style={{ display: 'flex', gap: 10 }}>
          {ZONES.map((z) => (
            <SelectCard
              key={z.id}
              selected={form.availability_zone === z.id}
              onClick={() => setField('availability_zone', z.id)}
              title={z.label}
            />
          ))}
        </div>
        <FieldErr msg={errors.availability_zone} />
      </FormSection>

      {/* ── Section: SSH Key ── */}
      <FormSection label="SSH Key">
        {keysLoading ? (
          <Skeleton height={38} borderRadius="6px" />
        ) : sshKeys.length === 0 ? (
          <div style={{
            padding: '14px 16px',
            border: '1px dashed rgba(255,255,255,0.1)',
            borderRadius: 7,
            color: '#6b7280',
            fontSize: 13,
          }}>
            No SSH keys found.{' '}
            <a
              href="/ssh-keys"
              onClick={(e) => { e.preventDefault(); navigate('/ssh-keys'); }}
              style={{ color: '#60a5fa', textDecoration: 'none' }}
            >
              Add an SSH key
            </a>
            {' '}before creating an instance.
          </div>
        ) : (
          <select
            value={form.ssh_key_name}
            onChange={(e) => setField('ssh_key_name', e.target.value)}
            style={selectSt}
          >
            {sshKeys.map((k) => (
              <option key={k.id} value={k.name}>
                {k.name} · {(k.fingerprint ?? 'no-fingerprint').slice(0, 28)}…
              </option>
            ))}
          </select>
        )}
        <FieldErr msg={errors.ssh_key_name} />
      </FormSection>

      {/* ── Section: Name ── */}
      <FormSection label="Instance Name">
        <input
          type="text"
          value={form.name}
          onChange={(e) => setField('name', e.target.value)}
          onKeyDown={(e) => { if (e.key === 'Enter' && canSubmit) handleSubmit(); }}
          placeholder="e.g. web-prod-01"
          style={{
            ...inputSt,
            borderColor: errors.name ? 'rgba(248,113,113,0.45)' : undefined,
          }}
        />
        <div style={{ marginTop: 5, fontSize: 11, color: '#374151', fontFamily: 'IBM Plex Mono, monospace' }}>
          ^[a-z][a-z0-9-]{'{'/* eslint-disable-line */}0,61{'}'}[a-z0-9]$
        </div>
        <FieldErr msg={errors.name} />
      </FormSection>

      {/* ── Submit row ── */}
      <div style={{ display: 'flex', gap: 10, marginTop: 30 }}>
        <button
          onClick={handleSubmit}
          disabled={!canSubmit}
          style={{
            padding: '9px 22px',
            borderRadius: 7,
            border: 'none',
            background: canSubmit ? '#3b82f6' : 'rgba(59,130,246,0.25)',
            color: canSubmit ? '#fff' : 'rgba(255,255,255,0.35)',
            fontSize: 13,
            fontWeight: 500,
            cursor: canSubmit ? 'pointer' : 'not-allowed',
            fontFamily: 'IBM Plex Sans, sans-serif',
          }}
        >
          {submitting ? 'Creating…' : 'Create Instance'}
        </button>
        <button
          onClick={() => navigate('/instances')}
          style={{
            padding: '9px 18px',
            borderRadius: 7,
            border: '1px solid rgba(255,255,255,0.1)',
            background: 'transparent',
            color: '#6b7280',
            fontSize: 13,
            cursor: 'pointer',
            fontFamily: 'IBM Plex Sans, sans-serif',
          }}
        >
          Cancel
        </button>
      </div>
    </div>
  );
}

// ── Sub-components ────────────────────────────────────────────────────────────

function FormSection({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div style={{ marginBottom: 26 }}>
      <div style={{
        fontSize: 11,
        fontWeight: 600,
        color: '#4b5563',
        letterSpacing: '0.09em',
        marginBottom: 10,
        fontFamily: 'IBM Plex Sans, sans-serif',
      }}>
        {label.toUpperCase()}
      </div>
      {children}
    </div>
  );
}

function SelectCard({
  selected,
  onClick,
  title,
  sub,
}: {
  selected: boolean;
  onClick: () => void;
  title: string;
  sub?: string;
}) {
  return (
    <button
      onClick={onClick}
      style={{
        padding: '13px 15px',
        border: selected ? '1px solid #3b82f6' : '1px solid rgba(255,255,255,0.09)',
        borderRadius: 7,
        background: selected ? 'rgba(59,130,246,0.09)' : 'rgba(255,255,255,0.02)',
        cursor: 'pointer',
        textAlign: 'left',
        transition: 'border-color 0.12s, background 0.12s',
      }}
    >
      <div style={{
        color: selected ? '#93c5fd' : '#d1d5db',
        fontSize: 13,
        fontWeight: 500,
        marginBottom: sub ? 3 : 0,
        fontFamily: 'IBM Plex Sans, sans-serif',
      }}>
        {title}
      </div>
      {sub && (
        <div style={{
          color: '#6b7280',
          fontSize: 11,
          fontFamily: 'IBM Plex Mono, monospace',
        }}>
          {sub}
        </div>
      )}
    </button>
  );
}

function FieldErr({ msg }: { msg?: string }) {
  if (!msg) return null;
  return (
    <div style={{ marginTop: 5, color: '#f87171', fontSize: 12, fontFamily: 'IBM Plex Sans, sans-serif' }}>
      {msg}
    </div>
  );
}

const inputSt: React.CSSProperties = {
  width: '100%',
  padding: '9px 13px',
  borderRadius: 6,
  border: '1px solid rgba(255,255,255,0.11)',
  background: 'rgba(255,255,255,0.04)',
  color: '#f3f4f6',
  fontSize: 13,
  fontFamily: 'IBM Plex Mono, monospace',
  outline: 'none',
  boxSizing: 'border-box',
};

const selectSt: React.CSSProperties = {
  ...inputSt,
  appearance: 'none' as const,
  cursor: 'pointer',
  paddingRight: 28,
};
