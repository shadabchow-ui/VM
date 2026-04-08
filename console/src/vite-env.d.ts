/// <reference types="vite/client" />

interface ImportMetaEnv {
  readonly VITE_API_URL?: string;
  readonly VITE_API_TARGET?: string;
  readonly VITE_PRINCIPAL_ID?: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}
