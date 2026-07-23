export interface Session {
  csrf: string;
  user: string;
  fileRoot: string;
}

export interface ServiceStatus {
  name: "nginx" | "php" | "mysql" | "ftp" | string;
  service: string;
  installed: boolean;
  running: boolean;
  enabled: boolean;
  version: string;
}

export interface ResourceUsage {
  total: number;
  used: number;
  available: number;
}

export interface SystemOverview {
  hostname: string;
  publicIpAddress: string;
  ipAddresses: string[];
  kernel: string;
  uptimeSeconds: number;
  cpu: {
    cores: number;
    usagePercent: number;
    loadAverage: number[];
  };
  memory: ResourceUsage;
  disk: ResourceUsage;
  network: {
    receivedBytes: number;
    transmittedBytes: number;
  };
}

export interface Overview {
  platform: string;
  services: ServiceStatus[];
  system: SystemOverview;
}

export interface UpdateStatus {
  currentVersion: string;
  latestVersion?: string;
  updateAvailable: boolean;
  releaseUrl?: string;
  publishedAt?: string;
  checkedAt: string;
  error?: string;
}
