import { http, HttpResponse, type HttpResponseResolver } from 'msw';
import { API_URL } from '@/api/config';

export const projectsRead = (resolve: HttpResponseResolver) =>
  http.get(`${API_URL}/projects`, resolve);

export const projectsList = (names: { id: string; owner: string; name: string }[]) =>
  projectsRead(() => HttpResponse.json(names));
