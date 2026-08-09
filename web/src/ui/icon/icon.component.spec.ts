import { TestBed } from '@angular/core/testing';

import { IconComponent } from './icon.component';
import { ICONS } from './icons';

function render(name: string, size?: number): HTMLElement {
  const fixture = TestBed.createComponent(IconComponent);
  fixture.componentRef.setInput('name', name);
  if (size !== undefined) fixture.componentRef.setInput('size', size);
  fixture.detectChanges();
  return fixture.nativeElement as HTMLElement;
}

function glyph(name: string): string {
  const svg = render(name).querySelector('svg');
  expect(svg).withContext(`no svg rendered for "${name}"`).not.toBeNull();
  return svg!.innerHTML;
}

describe('icon', () => {
  it('draws the glyph registered under the requested name', () => {
    // Two distinct registry entries must draw two distinct pictures: a
    // registry where every name resolved to the same shape would still render
    // and still pass a "renders something" assertion.
    expect(glyph('trash-2')).not.toBe(glyph('plus'));
  });

  // lucide-icon THROWS ("No icon name or image has been provided") when [img]
  // is undefined, which in a template takes the whole view down. The `??
  // ICONS['circle']` fallback is what turns a name missing from the registry
  // into a visible dot instead — a typo in one icon name must not blank a page.
  it('falls back to a plain circle for a name absent from the registry', () => {
    expect(() => render('no-such-icon')).not.toThrow();
    expect(glyph('no-such-icon')).toBe(glyph('circle'));
  });

  // The fallback above is only a fallback while this entry exists; dropping it
  // from the registry to save a few bytes would make every missing icon throw.
  it('keeps the fallback entry in the registry', () => {
    expect(ICONS['circle']).toBeDefined();
  });

  // Icons are decorative (design-system §3.1): the adjacent text carries the
  // meaning, so a screen reader must never announce the glyph.
  it('stays out of the accessibility tree', () => {
    expect(render('plus').querySelector('lucide-icon')!.getAttribute('aria-hidden')).toBe('true');
  });

  it('honours the requested size', () => {
    const svg = render('plus', 28).querySelector('svg')!;
    expect(svg.getAttribute('width')).toBe('28');
    expect(svg.getAttribute('height')).toBe('28');
  });
});
