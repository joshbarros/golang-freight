import { HttpClient } from '@angular/common/http';
import { Injectable, inject } from '@angular/core';
import { firstValueFrom } from 'rxjs';
import { Booking, LogEntry, Sailing } from './models';

export interface NewSailing {
  code: string;
  vessel: string;
  origin: string;
  destination: string;
  capacityTeu: number;
}

@Injectable({ providedIn: 'root' })
export class ApiService {
  private readonly http = inject(HttpClient);

  sailings = () => firstValueFrom(this.http.get<Sailing[]>('/api/sailings'));
  openSailing = (body: NewSailing) =>
    firstValueFrom(this.http.post<Sailing>('/api/sailings', body));

  bookings = (sailingCode: string) =>
    firstValueFrom(this.http.get<Booking[]>('/api/bookings', { params: { sailingCode } }));
  book = (body: { sailingCode: string; reference: string; teu: number }) =>
    firstValueFrom(this.http.post<Booking>('/api/bookings', body));
  reverse = (id: string) =>
    firstValueFrom(this.http.post<Booking>(`/api/bookings/${id}/reversal`, {}));
  events = (id: string) => firstValueFrom(this.http.get<LogEntry[]>(`/api/bookings/${id}/events`));
}
