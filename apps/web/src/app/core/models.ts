export interface Sailing {
  id: string;
  code: string;
  vessel: string;
  origin: string;
  destination: string;
  departure: string;
  capacityTeu: number;
  remainingTeu: number;
}

export type BookingStatus = 'Requested' | 'Held' | 'Issued' | 'Reversed' | 'Rejected';

export interface Booking {
  id: string;
  sailingCode: string;
  ownerId: string;
  reference: string;
  teu: number;
  feeCents: number;
  status: BookingStatus;
  reason: string | null;
  billOfLadingNumber: string | null;
  createdAt: string;
  updatedAt: string;
  capacityReleasedAt: string | null;
}

export interface LogEntry {
  id: string;
  type: string;
  module: string;
  occurredAt: string;
}

export interface UserInfo {
  id: string;
  email: string;
  roles: string[];
}

export interface AuthResponse {
  accessToken: string;
  expiresAt: string;
  user: UserInfo;
}
