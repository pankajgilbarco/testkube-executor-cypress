describe('Test POST create Alert Audit Record', () => {
  it('AT-T6054', function () {
    cy.request('GET', new URL(`/im/v1/alerts/63403168f8985fde243c161e`, `http://http://10.1.0.221:8887`).toString(),
    ).then((response) => {
      //cy.log(JSON.stringify(response))
      expect(response.status).to.eq(200);
      expect(response.body.alert._id).to.eq('63403168f8985fde243c161e');
      expect(response.body.alert.dedupeId).to.eq('Last Seen-Device Disconnected-4014-3006');
      //To-Do Assertions
    });
  });

});
