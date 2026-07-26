import { parseDomainRows, serializeDomainRows } from './config-fields.component';

describe('routing table parse/serialize', () => {
  it('groups domains that share a target port into one row', () => {
    const rows = parseDomainRows('a.example.com:1337\nb.example.com:1337\nc.example.com');
    expect(rows).toEqual([
      { domains: 'a.example.com, b.example.com', port: '1337' },
      { domains: 'c.example.com', port: '' },
    ]);
  });

  it('serializes a comma-separated row to one line per fqdn with the port', () => {
    const out = serializeDomainRows([{ domains: 'a.example.com, b.example.com', port: '8080' }]);
    expect(out).toBe('a.example.com:8080\nb.example.com:8080');
  });

  it('keeps a path (port precedes path) and ignores a non-numeric suffix', () => {
    const rows = parseDomainRows('app.example.com:3000/api\nhost.example.com:notaport');
    expect(rows).toEqual([
      { domains: 'app.example.com/api', port: '3000' },
      { domains: 'host.example.com:notaport', port: '' },
    ]);
  });

  it('round-trips a mixed set', () => {
    const text = 'a.example.com:1337\nb.example.com:1337\nc.example.com/admin';
    expect(serializeDomainRows(parseDomainRows(text))).toBe(text);
  });

  it('drops blank rows and blank domains', () => {
    expect(serializeDomainRows([{ domains: '  ,  ', port: '80' }, { domains: '', port: '' }])).toBe(
      '',
    );
  });
});
